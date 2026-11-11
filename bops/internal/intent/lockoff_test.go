package intent_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// lock.mode none: a repository on one machine, with no git remote at all.
// Every git call is recorded, and none may happen.

type noGit struct {
	t     *testing.T
	calls [][]string
}

func (g *noGit) Run(_ context.Context, c lock.Cmd) (lock.Output, error) {
	g.calls = append(g.calls, c.Args)
	g.t.Errorf("lock.mode none ran git %v", c.Args)
	return lock.Output{Stderr: "fatal: 'origin' does not appear to be a git repository"}, errors.New("exit status 128")
}

type offRig struct {
	env   *intent.Env
	bd    *fakeBD
	store *runstate.Store
}

func lockOffRig(t *testing.T, run string, bd *fakeBD) *offRig {
	t.Helper()
	bareRemote(t) // isolates git config and state; this repository never uses it
	dir := filepath.Join(t.TempDir(), run)
	git(t, filepath.Dir(dir), "init", "-q", dir)
	s := openStore(t, dir, run)
	env := &intent.Env{BD: bd, Run: s.Run(), Store: s, Sleep: func(time.Duration) {}, LockOff: true,
		Lock: &lock.Lock{Git: &noGit{t: t}, Journal: s, Run: run, Actor: "tester"}}
	return &offRig{env: env, bd: bd, store: s}
}

func saysLockOff(t *testing.T, warnings []string) {
	t.Helper()
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "lock.mode is none") }) {
		t.Errorf("warnings %v do not say the lock is off", warnings)
	}
}

func TestLockOffClaimUsesTheLedgerAlone(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)

	out, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeClaimed || out.LedgerPending {
		t.Fatalf("%+v, %v", out, err)
	}
	saysLockOff(t, out.Warnings)
	b := bd.beads["x-1"]
	if b.Assignee != "tester" || b.Status != "in_progress" || !slices.Contains(b.Labels, "run:A") {
		t.Errorf("ledger = %+v", b)
	}
	if !slices.ContainsFunc(b.Comments, func(c string) bool { return strings.HasPrefix(c, "bops:claimed run=A ") }) {
		t.Errorf("comments = %v", b.Comments)
	}
	if bd.count("update x-1 --claim") != 1 || bd.count("dolt remote") != 0 {
		t.Errorf("calls = %v", bd.calls)
	}
	if e, ok := r.store.Entry("x-1"); !ok || !e.Legacy() {
		t.Errorf("journal = %+v, %v; want a hold with no ref", e, ok)
	}

	again, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || again.Outcome != intent.OutcomeHeld {
		t.Fatalf("repeat claim: %+v, %v", again, err)
	}
}

func TestLockOffClaimRefusesAnAssignedBead(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "other", "in_progress", []string{"run:B"}
	r := lockOffRig(t, "A", bd)

	out, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeRefused || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") {
		t.Errorf("calls = %v", bd.calls)
	}
}

// Two worktrees of one clone share the ledger, so bd's own compare-and-set on
// --claim decides; the loser is told who won.
func TestLockOffClaimLosesARaceOnTheLedger(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	bd.fail = func(args []string) error {
		if args[0] == "update" && slices.Contains(args, "--claim") {
			n := bd.beads["x-1"]
			n.Assignee, n.Status, n.Labels = "other", "in_progress", []string{"run:B"}
			return errors.New("issue x-1 already claimed by other")
		}
		return nil
	}

	out, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeLost || out.Holder == nil || out.Holder.Actor != "other" || out.Holder.Run != "B" {
		t.Fatalf("%+v (holder %+v), %v", out, out.Holder, err)
	}
	if _, ok := r.store.Entry("x-1"); ok {
		t.Error("a lost claim was journaled")
	}
}

func TestLockOffClaimWithNoDoltRemoteStillClaims(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	bd.fail = func(args []string) error {
		if args[0] == "dolt" {
			return errors.New("no remote configured")
		}
		return nil
	}
	out, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeClaimed || bd.beads["x-1"].Assignee != "tester" {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestLockOffReleaseWritesTheLedgerOnly(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	if out, err := intent.Claim(bg, r.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("claim: %+v, %v", out, err)
	}

	out, err := intent.Release(bg, r.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeReleased || out.RefPending {
		t.Fatalf("%+v, %v", out, err)
	}
	saysLockOff(t, out.Warnings)
	b := bd.beads["x-1"]
	if b.Assignee != "" || b.Status != "open" || slices.Contains(b.Labels, "run:A") || !slices.Contains(b.Labels, "bops:hold") {
		t.Errorf("ledger = %+v", b)
	}
	if _, ok := r.store.Entry("x-1"); ok {
		t.Error("hold not dropped")
	}
}

// A hold journaled under lock.mode ref still has a sha. With the lock off it
// is released from the ledger, never by reading or deleting its ref.
func TestLockOffReleaseIgnoresARefLeftFromModeRef(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A"}
	r := lockOffRig(t, "A", bd)
	if err := r.store.Confirm("x-1", "0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Release(bg, r.env, "x-1", "blocked")
	if err != nil || out.Outcome != intent.OutcomeReleased || bd.beads["x-1"].Status != "blocked" {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestLockOffReleaseIsLostWhenTheLedgerShowsAnotherRun(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	if out, err := intent.Claim(bg, r.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("claim: %+v, %v", out, err)
	}
	n := bd.beads["x-1"]
	n.Assignee, n.Labels = "other", []string{"run:B"} // taken over with claim --force
	calls := len(bd.calls)

	out, err := intent.Release(bg, r.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeLost || out.Holder == nil || out.Holder.Run != "B" {
		t.Fatalf("%+v, %v", out, err)
	}
	for _, c := range bd.calls[calls:] {
		if c[0] == "update" || c[0] == "label" || (c[0] == "comments" && c[1] == "add") {
			t.Errorf("a lost release wrote %v", c)
		}
	}
	if _, ok := r.store.Entry("x-1"); ok {
		t.Error("lost hold not dropped")
	}
}

func TestLockOffForceTakesAStaleLedgerClaim(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "other", "in_progress", []string{"run:B"}
	n.Updated = time.Now().Add(-3 * time.Hour)
	r := lockOffRig(t, "A", bd)

	out, err := intent.Claim(bg, r.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	saysLockOff(t, out.Warnings)
	b := bd.beads["x-1"]
	if b.Assignee != "tester" || !slices.Contains(b.Labels, "run:A") || slices.Contains(b.Labels, "run:B") {
		t.Errorf("ledger = %+v", b)
	}
	if !slices.ContainsFunc(b.Comments, func(c string) bool { return strings.Contains(c, "takeover=B") }) {
		t.Errorf("comments = %v", b.Comments)
	}
	if e, ok := r.store.Entry("x-1"); !ok || !e.Legacy() {
		t.Errorf("journal = %+v, %v", e, ok)
	}
}

func TestLockOffForceRefusesAFreshLedgerClaim(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "other", "in_progress", []string{"run:B"}
	n.Updated = time.Now().Add(-time.Minute)
	r := lockOffRig(t, "A", bd)

	out, err := intent.Claim(bg, r.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestLockOffResumeIsRefused(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Status, n.Labels = "in_progress", []string{"bops:landing"}
	r := lockOffRig(t, "A", bd)

	out, err := intent.ClaimResume(bg, r.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Reason, "lock.mode") {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") {
		t.Errorf("calls = %v", bd.calls)
	}
}

func TestLockOffStaleAgesFromTheLedger(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A"}
	n.Updated = time.Now().Add(-3 * time.Hour)
	r := lockOffRig(t, "A", bd)

	out, err := intent.Stale(bg, r.env, intent.StaleOpts{AllMachines: true})
	if err != nil || len(out.Stale) != 1 || out.Stale[0].Source != "ledger" || len(out.Orphans) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	saysLockOff(t, out.Warnings)
}

func TestLockOffNextReadsNoRefs(t *testing.T) {
	r := lockOffRig(t, "A", newBD())
	out, err := intent.Next(bg, r.env, intent.NextOpts{})
	if err != nil || len(out.Candidates) != 2 {
		t.Fatalf("%+v, %v", out, err)
	}
	saysLockOff(t, out.Warnings)
}

// A hold with a sha from lock.mode ref is not reported lost because no ref
// is read: sync keeps every hold that is not stranded.
func TestLockOffSessionStartKeepsHolds(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A"}
	r := lockOffRig(t, "A", bd)
	if err := r.store.Confirm("x-1", "0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}

	out, err := intent.SessionStart(bg, r.env)
	if err != nil || !out.Synced || len(out.LostClaims) != 0 || !slices.Equal(out.Mine, []string{"x-1"}) {
		t.Fatalf("%+v, %v", out, err)
	}
	saysLockOff(t, out.Warnings)
	if b := bd.beads["x-1"]; b.Assignee != "tester" || b.Status != "in_progress" {
		t.Errorf("ledger = %+v", b)
	}
}

func TestLockOffLandAndSweep(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	if out, err := intent.Claim(bg, r.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("claim: %+v, %v", out, err)
	}
	land, err := intent.Land(bg, r.env, "x-1", intent.LandOpts{Ref: refMerged, SHA: sha})
	if err != nil || land.Outcome != intent.OutcomeLanded {
		t.Fatalf("land: %+v, %v", land, err)
	}
	saysLockOff(t, land.Warnings)
	if out, err := intent.Release(bg, r.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	if b := bd.beads["x-1"]; b.Status != "in_progress" || !slices.Contains(b.Labels, "bops:landing") {
		t.Fatalf("ledger after release done = %+v", b)
	}

	sw, err := intent.Sweep(bg, r.env, &fakeForge{merged: map[string]forge.Merged{refMerged: forge.Yes}}, false)
	if err != nil || !slices.Equal(sw.Closed, []string{"x-1"}) {
		t.Fatalf("sweep: %+v, %v", sw, err)
	}
	saysLockOff(t, sw.Warnings)
	if bd.beads["x-1"].Status != "closed" {
		t.Errorf("ledger = %+v", bd.beads["x-1"])
	}
}

func TestLockOffLandIsLostWhenTheLedgerShowsAnotherRun(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	if out, err := intent.Claim(bg, r.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("claim: %+v, %v", out, err)
	}
	n := bd.beads["x-1"]
	n.Assignee, n.Labels = "other", []string{"run:B"}

	land, err := intent.Land(bg, r.env, "x-1", intent.LandOpts{Ref: refMerged, SHA: sha})
	if err != nil || land.Outcome != intent.OutcomeLost || land.Holder == nil || land.Holder.Run != "B" {
		t.Fatalf("land: %+v, %v", land, err)
	}
	if n.Metadata != nil {
		t.Errorf("a lost land wrote metadata %s", n.Metadata)
	}
}

// lock.remote names the remote that holds claim refs. With the lock off it is
// never consulted: no git call, no Dolt remote comparison, no no_origin.
func TestLockOffIgnoresANonDefaultLockRemote(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	r.env.Lock.Remote = "upstream" // no such git remote exists
	g := r.env.Lock.Git.(*noGit)

	if out, err := intent.Next(bg, r.env, intent.NextOpts{}); err != nil {
		t.Fatalf("next: %+v, %v", out, err)
	}
	claim, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || claim.Outcome != intent.OutcomeClaimed {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	saysLockOff(t, claim.Warnings)
	if out, err := intent.SessionStart(bg, r.env); err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("session-start: %+v, %v", out, err)
	}
	if out, err := intent.Release(bg, r.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	// The Dolt remote list is read only to decide whether there is anywhere
	// to publish; it is never compared with a lock remote.
	if len(g.calls) != 0 {
		t.Errorf("git calls = %v; bd calls = %v", g.calls, bd.calls)
	}
}

// doltWithNoRemote makes the fake answer as bd 1.3.4 does with no Dolt remote
// configured: `dolt remote list --json` prints [], `dolt pull` exits 1 with
// "Error 1105: no remote", and `dolt push` prints a note and exits 0.
func doltWithNoRemote(bd *fakeBD) {
	bd.remotes = "[]"
	bd.fail = func(args []string) error {
		if args[0] == "dolt" && args[1] == "pull" {
			return errors.New("bd dolt pull: exit status 1: Error: fetch from origin/main: Error 1105: no remote")
		}
		return nil
	}
}

func saysUnpublished(t *testing.T, what string, warnings []string) {
	t.Helper()
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "no Dolt remote: ledger not published") }) {
		t.Errorf("%s: warnings %v do not say the ledger was not published", what, warnings)
	}
}

// A single-machine ledger with no Dolt remote has nothing to publish: every
// command that pulls or pushes skips that step with a warning, not an error.
func TestLockOffWithNoDoltRemotePublishesNothing(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	bd.add("x-2")
	r := lockOffRig(t, "A", bd)
	doltWithNoRemote(bd)
	for _, id := range []string{"x-1", "x-2"} {
		if out, err := intent.Claim(bg, r.env, id, false); err != nil || out.Outcome != intent.OutcomeClaimed {
			t.Fatalf("claim %s: %+v, %v", id, out, err)
		}
	}
	calls := len(bd.calls)

	rel, err := intent.Release(bg, r.env, "x-2", "scope")
	if err != nil || rel.Outcome != intent.OutcomeReleased || rel.LedgerPending {
		t.Fatalf("release: %+v, %v", rel, err)
	}
	saysUnpublished(t, "release", rel.Warnings)

	land, err := intent.Land(bg, r.env, "x-1", intent.LandOpts{Ref: refMerged, SHA: sha})
	if err != nil || land.Outcome != intent.OutcomeLanded {
		t.Fatalf("land: %+v, %v", land, err)
	}
	saysUnpublished(t, "land", land.Warnings)
	if done, err := intent.Release(bg, r.env, "x-1", "done"); err != nil || done.LedgerPending {
		t.Fatalf("release done: %+v, %v", done, err)
	}

	sw, err := intent.Sweep(bg, r.env, &fakeForge{merged: map[string]forge.Merged{refMerged: forge.Yes}}, false)
	if err != nil || !slices.Equal(sw.Closed, []string{"x-1"}) {
		t.Fatalf("sweep: %+v, %v", sw, err)
	}
	saysUnpublished(t, "sweep", sw.Warnings)

	sy, err := intent.Sync(bg, r.env, false)
	if err != nil || sy.Pulled || sy.Pushed {
		t.Fatalf("sync: %+v, %v", sy, err)
	}
	saysUnpublished(t, "sync", sy.Warnings)

	ss, err := intent.SessionStart(bg, r.env)
	if err != nil || !ss.Synced {
		t.Fatalf("session-start: %+v, %v", ss, err)
	}
	saysUnpublished(t, "session-start", ss.Warnings)

	for _, c := range bd.calls[calls:] {
		if c[0] == "dolt" && (c[1] == "pull" || c[1] == "push") {
			t.Errorf("with no Dolt remote, ran bd %v", c)
		}
	}
}

// With a Dolt remote configured, lock.mode none publishes as before, and a
// failed push or pull is the same failure it always was.
func TestLockOffWithADoltRemoteAFailedPublishStillFails(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	bd.add("x-2")
	bd.remotes = `[{"name":"origin","url":"git+https://example.com/ledger.git"}]`
	r := lockOffRig(t, "A", bd)
	for _, id := range []string{"x-1", "x-2"} {
		if out, err := intent.Claim(bg, r.env, id, false); err != nil || out.Outcome != intent.OutcomeClaimed {
			t.Fatalf("claim %s: %+v, %v", id, out, err)
		}
	}
	failing := "push"
	bd.fail = func(args []string) error {
		if args[0] == "dolt" && args[1] == failing {
			return errors.New("bd dolt " + failing + ": exit status 1: rejected")
		}
		return nil
	}

	if land, err := intent.Land(bg, r.env, "x-1", intent.LandOpts{Ref: refMerged, SHA: sha}); err == nil || !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("land: %+v, %v; want push failed", land, err)
	}
	if done, err := intent.Release(bg, r.env, "x-1", "done"); err != nil || !done.LedgerPending {
		t.Fatalf("release done: %+v, %v; want ledger_pending", done, err)
	}
	if sw, err := intent.Sweep(bg, r.env, &fakeForge{merged: map[string]forge.Merged{refMerged: forge.Yes}}, false); err == nil || !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("sweep: %+v, %v; want push failed", sw, err)
	}
	if rel, err := intent.Release(bg, r.env, "x-2", "scope"); err != nil || !rel.LedgerPending {
		t.Fatalf("release: %+v, %v; want ledger_pending", rel, err)
	}
	if sy, err := intent.Sync(bg, r.env, false); err == nil || sy.Pushed {
		t.Fatalf("sync: %+v, %v; want a push error", sy, err)
	}
	failing = "pull"
	if ss, err := intent.SessionStart(bg, r.env); err == nil || ss.Synced {
		t.Fatalf("session-start: %+v, %v; want a sync error", ss, err)
	}
	if bd.count("dolt push") == 0 {
		t.Errorf("never pushed: %v", bd.calls)
	}
}
