package intent_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// readyRows is a bd ready reply listing ids in order, all workable.
func readyRows(ids ...string) []byte {
	var rows []string
	for k, id := range ids {
		rows = append(rows, fmt.Sprintf(`{"id":%q,"title":"t","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-%02dT00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`, id, k+1))
	}
	return []byte("[" + strings.Join(rows, ",") + "]")
}

func workable(out *intent.NextOut) []string {
	var ids []string
	for _, c := range out.Candidates {
		if c.Workable {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// A claims x-1 on its machine. B's ledger lags: it has not pulled A's claim,
// and B has not synced since, so its copy of the refs is empty too. next must
// still leave x-1 out, because it reads the refs from the lock remote.
func TestNextSkipsAClaimTheLedgerHasNotCaughtUpWith(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1") // still open on B's replica
	bdB.add("x-2")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")

	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 5, IncludeUnworkable: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); len(got) != 1 || got[0] != "x-2" {
		t.Fatalf("workable = %v, want [x-2]", got)
	}
	for _, c := range out.Candidates {
		if c.ID == "x-1" && !strings.Contains(strings.Join(c.WhyNot, ";"), "W14: claimed by tester") {
			t.Errorf("x-1 why_not = %v", c.WhyNot)
		}
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// A ref B saw at its last sync and A has since released no longer holds the
// bead back: the fetch prunes it.
func TestNextOffersABeadReleasedSinceTheLastSync(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	if err := b.env.Lock.Fetch(bg); err != nil { // B's last sync saw the claim
		t.Fatal(err)
	}
	if _, err := a.env.Lock.Delete(bg, "x-1", liveSHA(t, a, "x-1")); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); len(got) != 2 || got[0] != "x-1" {
		t.Errorf("workable = %v, want x-1 first", got)
	}
}

// Offline, next still answers from the last sync's copy and says so.
func TestNextWarnsAndUsesTheLastSyncWhenTheFetchFails(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	if err := b.env.Lock.Fetch(bg); err != nil {
		t.Fatal(err)
	}
	b.git.offline = true
	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); len(got) != 1 || got[0] != "x-2" {
		t.Errorf("workable = %v, want [x-2] from the last sync's copy", got)
	}
	if !strings.Contains(strings.Join(out.Warnings, ";"), "claim refs not refreshed") {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// With Local (session-start, right after its sync) next does not fetch.
func TestNextLocalDoesNotFetch(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 5, Local: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); len(got) != 2 {
		t.Errorf("workable = %v, want both: Local reads only the local copy", got)
	}
}

// A plain claim that loses names the holder and the bead to try instead.
func TestLostClaimNamesTheHolderAndTheNextCandidate(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	bdB.add("x-2")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")

	out, err := intent.Claim(bg, b.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeLost {
		t.Fatalf("%+v, %v", out, err)
	}
	if out.Holder == nil || out.Holder.Run != "A" {
		t.Errorf("holder = %+v", out.Holder)
	}
	if out.NextCandidate != "x-2" {
		t.Errorf("next_candidate = %q, want x-2", out.NextCandidate)
	}
}

// A claim that wins names no next candidate.
func TestClaimedNamesNoNextCandidate(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	if out := mustClaim(t, a, "x-1"); out.NextCandidate != "" {
		t.Errorf("next_candidate = %q", out.NextCandidate)
	}
}

// B's next ran before A's claim reached the remote, so B tries x-1 and
// loses the race; claim --next moves on and takes x-2.
func TestClaimNextTakesTheNextCandidateOnALostRace(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	bdB.add("x-2")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	b.git.beforePush = func() { mustClaim(t, a, "x-1") } // A wins between B's next and B's push

	out, err := intent.ClaimNext(bg, b.env)
	if err != nil || out.Outcome != intent.OutcomeClaimed || out.ID != "x-2" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].ID != "x-1" || out.Skipped[0].Outcome != intent.OutcomeLost ||
		out.Skipped[0].Holder == nil || out.Skipped[0].Holder.Run != "A" {
		t.Errorf("skipped = %+v", out.Skipped)
	}
	if _, ok := b.store.Entry("x-2"); !ok {
		t.Error("B must hold x-2")
	}
	if _, ok := b.store.Entry("x-1"); ok {
		t.Error("B must keep no hold on x-1")
	}
	if bdB.wrote("x-1") {
		t.Errorf("loser wrote x-1 to the ledger: %v", bdB.calls)
	}
}

// With a lagging ledger and nothing else free, claim --next does not try the
// held bead at all: next has already left it out.
func TestClaimNextRefusesWhenNothingIsFree(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	bdB.ready = readyRows("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	pushes := 0
	b.git.beforePush = func() { pushes++ }

	out, err := intent.ClaimNext(bg, b.env)
	if err != nil || out.Outcome != intent.OutcomeRefused || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if !strings.Contains(out.Reason, "no workable bead") || len(out.Skipped) != 0 {
		t.Errorf("reason %q, skipped %+v", out.Reason, out.Skipped)
	}
	if pushes != 0 {
		t.Errorf("claim --next pushed %d times for a held bead", pushes)
	}
}

// Candidates the ledger shows taken are refused, without a push; the walk
// stops after maxNextClaims and names the next untried one.
func TestClaimNextStopsAfterFiveTries(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	ids := []string{"x-1", "x-2", "x-3", "x-4", "x-5", "x-6"}
	for _, id := range ids {
		bd.add(id).Assignee = "someone" // bd ready lags: it still lists them
	}
	bd.ready = readyRows(ids...)
	a := client(t, remote, "A", bd)

	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 5 || out.Skipped[4].ID != "x-5" || out.Skipped[0].Outcome != intent.OutcomeRefused {
		t.Errorf("skipped = %+v", out.Skipped)
	}
	if out.NextCandidate != "x-6" || !strings.Contains(out.Reason, "stopped after 5 tries") {
		t.Errorf("next_candidate %q, reason %q", out.NextCandidate, out.Reason)
	}
}

// Every candidate refused, fewer than the cap: the reason says all were tried.
func TestClaimNextRefusesWhenEveryCandidateIsTaken(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Assignee = "someone"
	bd.add("x-2").Assignee = "someone"
	a := client(t, remote, "A", bd)

	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 2 || !strings.Contains(out.Reason, "all 2 workable candidates were skipped") || out.NextCandidate != "" {
		t.Errorf("%+v", out)
	}
}

// An unreachable remote stops the walk: the next candidate would meet it too.
func TestClaimNextStopsOnUnreachable(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	a.git.offline = true

	out, err := intent.ClaimNext(bg, a.env)
	if err == nil || out == nil || out.Outcome != intent.OutcomeUnreachable || out.ID != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 0 || bd.wrote("x-2") || bd.count("show x-2") != 0 {
		t.Errorf("walk went on past an unreachable remote: %+v, calls %v", out, bd.calls)
	}
	if !strings.Contains(strings.Join(out.Warnings, ";"), "claim refs not refreshed") {
		t.Errorf("next's warning lost: %v", out.Warnings)
	}
}

// A refused plain claim (the ledger shows it taken) names a bead to try too.
func TestRefusedClaimNamesTheNextCandidate(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Assignee = "someone"
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeRefused || out.NextCandidate != "x-2" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// If next cannot answer after a lost claim, the claim still answers lost,
// with no next_candidate and a warning that says why.
func TestLostClaimWithoutANextCandidateWarns(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	bdB.fail = func(args []string) error {
		if args[0] == "ready" {
			return errors.New("bd ready broke")
		}
		return nil
	}
	out, err := intent.Claim(bg, b.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeLost || out.NextCandidate != "" {
		t.Fatalf("%+v, %v", out, err)
	}
	if !strings.Contains(strings.Join(out.Warnings, ";"), "next candidate not found: bd ready broke") {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// With under nextClaimReserve of the budget left, claim --next tries no
// candidate, the first included, and names it instead.
func TestClaimNextStopsWhenTheBudgetIsLow(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Assignee = "someone"
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	out, err := intent.ClaimNext(ctx, a.env)
	if err != nil || out.Outcome != intent.OutcomeRefused || out.NextCandidate != "x-1" || !strings.Contains(out.Reason, "budget") {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 0 || bd.count("show") != 0 {
		t.Errorf("skipped %+v; calls %v", out.Skipped, bd.calls)
	}
}

// After a lost race, an unreachable remote stops the walk and the skipped
// list survives on the error's document.
func TestClaimNextKeepsSkippedWhenALaterClaimFails(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	bdB.add("x-2")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	b.git.beforePush = func() {
		mustClaim(t, a, "x-1")
		b.git.beforePush = func() { b.git.offline = true }
	}
	out, err := intent.ClaimNext(bg, b.env)
	if lock.CodeOf(err) != lock.CodeUnreachable || out == nil || out.ID != "x-2" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].ID != "x-1" || out.Skipped[0].Outcome != intent.OutcomeLost {
		t.Errorf("skipped = %+v", out.Skipped)
	}
}

// A bead another bops process here is working on is skipped after a short
// wait, not the whole walk ended with ref_contention.
func TestClaimNextSkipsABusyBead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a.env.BeadWait = time.Minute // claim --next caps it
	start := time.Now()
	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out.Outcome != intent.OutcomeClaimed || out.ID != "x-2" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].ID != "x-1" || out.Skipped[0].Code != lock.CodeContention {
		t.Errorf("skipped = %+v", out.Skipped)
	}
	if time.Since(start) > 30*time.Second || a.env.BeadWait != time.Minute {
		t.Errorf("waited %v; BeadWait now %v", time.Since(start), a.env.BeadWait)
	}
}

// A candidate bd cannot show (gone since bd ready) is skipped.
func TestClaimNextSkipsAVanishedBead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-2") // x-1 is in bd ready but bd show has no such bead
	a := client(t, remote, "A", bd)
	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out.Outcome != intent.OutcomeClaimed || out.ID != "x-2" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].ID != "x-1" || out.Skipped[0].Outcome != intent.OutcomeError {
		t.Errorf("skipped = %+v", out.Skipped)
	}
}

// When bd fails for every candidate, that is bd failing: an error, not a
// refused verdict.
func TestClaimNextReturnsTheErrorWhenEveryCandidateFails(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd) // bd ready lists x-1 and x-2, bd show knows neither
	out, err := intent.ClaimNext(bg, a.env)
	if err == nil || out != nil {
		t.Fatalf("%+v, %v", out, err)
	}
}

// next failing (bd ready) is an error.
func TestClaimNextFailsWhenNextFails(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.fail = func(args []string) error {
		if args[0] == "ready" {
			return errors.New("bd ready broke")
		}
		return nil
	}
	a := client(t, remote, "A", bd)
	if out, err := intent.ClaimNext(bg, a.env); err == nil || out != nil {
		t.Fatalf("%+v, %v", out, err)
	}
}

// In lock-off mode a warning next and each claim both give appears once.
func TestClaimNextSaysEachWarningOnce(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Assignee = "someone"
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	a.env.LockOff = true
	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out.ID != "x-2" {
		t.Fatalf("%+v, %v", out, err)
	}
	n := 0
	for _, w := range out.Warnings {
		if w == lock.OffWarning {
			n++
		}
	}
	if n != 1 || slices.Contains(out.Warnings, "") {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// A run that already holds a bead (a retry whose first reply was lost, or a
// run shared with an orchestrator) is refused, naming it, and takes nothing.
func TestClaimNextRefusesARunThatAlreadyHoldsABead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	bd.ready = readyRows("x-2") // x-1 is in progress now, so bd ready drops it
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Reason, "already holds x-1") {
		t.Fatalf("%+v, %v", out, err)
	}
	if _, ok := a.store.Entry("x-2"); ok || bd.wrote("x-2") {
		t.Error("claim --next must not take a second bead")
	}
}

// A bead gone from the ledger plus busy beads is a verdict, not bd failing.
func TestClaimNextContentionIsNotABDFailure(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-2") // x-1 is in bd ready but bd show has no such bead
	a := client(t, remote, "A", bd)
	unlock, err := openStore(t, a.dir, "A").LockBead("x-2")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := intent.ClaimNext(bg, a.env)
	if err != nil || out == nil || out.Outcome != intent.OutcomeRefused || len(out.Skipped) != 2 {
		t.Fatalf("%+v, %v", out, err)
	}
}

// bd failing for every candidate is an error even when the try cap stops
// the walk first.
func TestClaimNextReturnsTheErrorWhenEveryTryFailsInBD(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.ready = readyRows("x-1", "x-2", "x-3", "x-4", "x-5", "x-6") // bd show knows none
	a := client(t, remote, "A", bd)
	if out, err := intent.ClaimNext(bg, a.env); err == nil || out != nil {
		t.Fatalf("%+v, %v", out, err)
	}
}
