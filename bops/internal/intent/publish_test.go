package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// allEvents is every line of every clone's events.jsonl under the test's
// XDG_STATE_HOME, in file order.
func allEvents(t *testing.T) []runstate.Event {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "events.jsonl"))
	var out []runstate.Event
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var ev runstate.Event
			if json.Unmarshal([]byte(line), &ev) == nil {
				out = append(out, ev)
			}
		}
	}
	return out
}

// eventsAt is every events.jsonl line with step.
func eventsAt(t *testing.T, step string) []runstate.Event {
	t.Helper()
	var out []runstate.Event
	for _, ev := range allEvents(t) {
		if ev.Step == step {
			out = append(out, ev)
		}
	}
	return out
}

// claim, claim --force and claim --resume under lock.mode ref push nothing,
// even with a push that would always fail.
func TestNoClaimPathPushesUnderTheLock(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	bdB.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt push" {
			return errors.New("non-fast-forward")
		}
		return nil
	}
	b.env.Lock.Machine = "far" // B is another machine, so A can resume from its own
	if out := mustForce(t, b, "x-1"); out.LedgerPending || out.Negative() {
		t.Fatalf("force: %+v", out)
	}
	if bdB.count("dolt push") != 0 || bdB.count("dolt commit") != 0 {
		t.Errorf("claim --force pushed: %v", bdB.calls)
	}
	// A resumes the bead on its own machine: same actor, its own ledger.
	a.bd.calls = nil
	out, err := intent.ClaimResume(bg, a.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeClaimed || out.LedgerPending {
		t.Fatalf("resume: %+v, %v", out, err)
	}
	if a.bd.count("dolt push") != 0 {
		t.Errorf("claim --resume pushed: %v", a.bd.calls)
	}
}

// lock.mode none has no ref: the ledger is the only record of a claim, so a
// claim there still commits and pushes.
func TestLockOffClaimStillPushes(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	bd.remotes = `[{"name":"origin","url":"git+file:///nowhere"}]`
	r := lockOffRig(t, "A", bd)
	if out, err := intent.Claim(bg, r.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.count("dolt push") != 1 {
		t.Errorf("pushes = %d, want 1: %v", bd.count("dolt push"), bd.calls)
	}
	committedBeforeEachPush(t, bd)
}

// A claim whose pull fails goes on with a warning, logs a ledger_pull event
// with outcome failed, and leaves a record status reports (and doctor:
// PullFailureWarning is its check) until a pull succeeds.
func TestClaimPullFailureIsAnEventThatStatusReports(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	down := true
	bd.fail = func(args []string) error {
		if down && strings.Join(args, " ") == "dolt pull" {
			return errors.New("bd dolt pull: exit status 1: remote unreachable")
		}
		return nil
	}
	out := mustClaim(t, a, "x-1")
	if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.HasPrefix(w, "ledger not pulled: ") }) {
		t.Errorf("warnings = %v", out.Warnings)
	}
	evs := eventsAt(t, "ledger_pull")
	if len(evs) != 1 || evs[0].Outcome != "failed" || evs[0].Command != "claim" || evs[0].Bead != "x-1" || !strings.Contains(evs[0].Detail, "remote unreachable") {
		t.Fatalf("ledger_pull events = %+v", evs)
	}
	st, err := intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if st.LedgerPullFailed == nil || st.LedgerPullFailed.Bead != "x-1" || st.LedgerPullFailed.Next == "" ||
		!slices.ContainsFunc(st.Warnings, func(w string) bool {
			return strings.HasPrefix(w, "ledger not pulled: bops claim's ledger pull for x-1")
		}) {
		t.Errorf("status = %+v", st)
	}
	if w := intent.PullFailureWarning(a.dir); !strings.Contains(w, "x-1") || !strings.Contains(w, "remote unreachable") {
		t.Errorf("doctor warning = %q", w)
	}

	down = false
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := intent.Status(bg, a.env, intent.StatusOpts{}); st.LedgerPullFailed != nil {
		t.Errorf("record outlived a pull that succeeded: %+v", st.LedgerPullFailed)
	}
	if w := intent.PullFailureWarning(a.dir); w != "" {
		t.Errorf("doctor warning = %q", w)
	}
	if evs := eventsAt(t, "ledger_pull"); len(evs) != 2 || evs[1].Outcome != "recovered" {
		t.Errorf("ledger_pull events = %+v", evs)
	}
}

// A pull that succeeds keeps the record while the recorded bead fails the
// re-check the claim skipped (another run's claim, or closed), and clears it
// once the bead passes.
func TestAPullClearsTheRecordOnlyOnceTheBeadPassesTheRecheck(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	down := true
	bd.fail = func(args []string) error {
		if down && strings.Join(args, " ") == "dolt pull" {
			return errors.New("remote unreachable")
		}
		return nil
	}
	mustClaim(t, a, "x-1")
	down = false
	n := bd.beads["x-1"]
	n.Labels = append(n.Labels, "run:OTHER") // what the skipped re-check would have seen
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	// sync's own display repair removes run:OTHER after its pull, so the
	// record stays from this sync and goes at the next pull.
	if f, _ := a.store.LedgerPullFailure(); f == nil {
		t.Fatal("record cleared although the bead showed another run's claim at the pull")
	}
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if f, _ := a.store.LedgerPullFailure(); f != nil {
		t.Errorf("record kept after the bead passed: %+v", f)
	}
	n.Status = "closed"
	if err := a.store.SetLedgerPullFailure(&runstate.LedgerPullFailure{At: time.Now().Add(-time.Minute), Command: "claim", Run: "A", Bead: "x-1", Detail: "x"}); err != nil {
		t.Fatal(err)
	}
	_, _ = intent.Sync(bg, a.env, false)
	if f, _ := a.store.LedgerPullFailure(); f == nil {
		t.Error("record cleared for a closed bead the run still holds")
	}
	// Once the run no longer holds the bead there is nothing to check.
	n.Status = "in_progress"
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	n.Status = "closed"
	if err := a.store.SetLedgerPullFailure(&runstate.LedgerPullFailure{At: time.Now().Add(-time.Minute), Command: "claim", Run: "A", Bead: "x-1", Detail: "x"}); err != nil {
		t.Fatal(err)
	}
	_, _ = intent.Sync(bg, a.env, false)
	if f, _ := a.store.LedgerPullFailure(); f != nil {
		t.Errorf("record kept for a bead the run no longer holds: %+v", f)
	}
}

// claim --force under the lock fails on a failed pull: the pull is logged,
// but no record is left, since there is no claim to check.
func TestClaimForcePullFailureIsLoggedNotRecorded(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	bdB.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("remote unreachable")
		}
		return nil
	}
	if _, err := intent.Claim(bg, b.env, "x-1", true); err == nil {
		t.Fatal("want the pull's error")
	}
	if evs := eventsAt(t, "ledger_pull"); len(evs) != 1 || evs[0].Command != "claim --force" || evs[0].Outcome != "failed" {
		t.Errorf("events = %+v", evs)
	}
	if f, err := b.store.LedgerPullFailure(); err != nil || f != nil {
		t.Errorf("record = %+v, %v; want none", f, err)
	}
}

// With the lock off claim --force carries on past a failed pull, so it is
// logged and recorded under its own name.
func TestLockOffClaimForcePullFailureNamesForce(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Status, n.Assignee, n.Labels = "in_progress", "someone", []string{"run:OLD"}
	bd.remotes = `[{"name":"origin","url":"git+file:///nowhere"}]`
	r := lockOffRig(t, "A", bd)
	r.env.Now = func() time.Time { return time.Now().UTC().Add(3 * time.Hour) }
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("remote unreachable")
		}
		return nil
	}
	if out, err := intent.Claim(bg, r.env, "x-1", true); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	if evs := eventsAt(t, "ledger_pull"); len(evs) != 1 || evs[0].Command != "claim --force" {
		t.Errorf("events = %+v", evs)
	}
	if f, _ := r.store.LedgerPullFailure(); f == nil || f.Command != "claim --force" {
		t.Errorf("record = %+v", f)
	}
}

// claim --resume carries on past a failed pull with a warning, and logs and
// records it.
func TestClaimResumePullFailureIsRecorded(t *testing.T) {
	a, b, _ := reapWorld(t, "x-1")
	b.env.Lock.Machine = "far"
	mustForce(t, b, "x-1")
	a.bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("remote unreachable")
		}
		return nil
	}
	out, err := intent.ClaimResume(bg, a.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeClaimed ||
		!slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.HasPrefix(w, "ledger not pulled: ") }) {
		t.Fatalf("%+v, %v", out, err)
	}
	evs := eventsAt(t, "ledger_pull")
	if len(evs) == 0 || evs[len(evs)-1].Command != "claim --resume" || evs[len(evs)-1].Outcome != "failed" {
		t.Errorf("events = %+v", evs)
	}
	if f, _ := a.store.LedgerPullFailure(); f == nil || f.Command != "claim --resume" || f.Bead != "x-1" {
		t.Errorf("record = %+v", f)
	}
}

// A pull that succeeds keeps a record of a pull that started after it
// (another process sharing the clone met that failure).
func TestASucceedingPullKeepsANewerPullFailure(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	newer := runstate.LedgerPullFailure{At: time.Now().Add(time.Hour), Command: "claim", Bead: "x-9", Detail: "later"}
	if err := a.store.SetLedgerPullFailure(&newer); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if f, _ := a.store.LedgerPullFailure(); f == nil || f.Bead != "x-9" {
		t.Errorf("record = %+v, want the newer one kept", f)
	}
	if evs := eventsAt(t, "ledger_pull"); len(evs) != 0 {
		t.Errorf("events = %+v, want no recovered line", evs)
	}
}

// A push that meets a ledger already stuck on a conflict makes no try and
// logs that: failed, the conflict's code, no tries.
func TestAPushOnAStuckLedgerLogsNoTries(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("Error: merge origin/main: merge conflicts in issues require operator resolution; merge aborted and working set restored")
		}
		return nil
	}
	a.env.Command = "sync"
	_, _ = intent.Sync(bg, a.env, false) // its pull meets the conflict; its push then makes no try
	var pushes []runstate.Event
	for _, ev := range eventsAt(t, "ledger_push") {
		if ev.Command == "sync" {
			pushes = append(pushes, ev)
		}
	}
	for _, ev := range pushes {
		if ev.Outcome != "failed" || ev.Code != "ledger_conflict" || ev.Tries != 0 {
			t.Errorf("push event = %+v", ev)
		}
	}
}

// land, release and sweep log every ledger push's outcome and tries.
func TestLandReleaseAndSweepLogEachPush(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	mustClaim(t, a, "x-2")
	if n := len(eventsAt(t, "ledger_push")); n != 0 {
		t.Fatalf("claims logged %d pushes", n)
	}

	rejectPushes(bd, 2)
	a.env.Command = "land"
	if _, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA}); err != nil {
		t.Fatal(err)
	}
	rejectPushes(bd, 100)
	a.env.Command = "release"
	if out, err := intent.Release(bg, a.env, "x-2", "scope"); err != nil || !out.LedgerPending {
		t.Fatalf("%+v, %v", out, err)
	}
	evs := eventsAt(t, "ledger_push")
	b, f := sweepFixture()
	env := sweepEnv(t, b) // a state directory of its own
	env.Command = "sweep"
	if _, err := intent.Sweep(context.Background(), env, f, false); err != nil {
		t.Fatal(err)
	}
	evs = append(evs, eventsAt(t, "ledger_push")...)
	type row struct {
		cmd, outcome string
		tries        int
	}
	var got []row
	for _, ev := range evs {
		got = append(got, row{ev.Command, ev.Outcome, ev.Tries})
		if ev.Outcome == "failed" && !strings.Contains(ev.Detail, "non-fast-forward") {
			t.Errorf("failed push without its error: %+v", ev)
		}
	}
	want := []row{{"land", "pushed", 3}, {"release", "failed", 5}, {"sweep", "pushed", 1}}
	if !slices.Equal(got, want) {
		t.Errorf("ledger_push events = %+v, want %+v", got, want)
	}
}

// status reports, under lock.mode ref, how far the local ledger is from its
// Dolt remote: claims not yet published, a diverged ledger, uncommitted
// rows, and counts from the last fetch when the remote did not answer.
func TestStatusReportsAnUnpublishedLedger(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")

	st, err := intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if st.LedgerPublish == nil || len(st.LedgerPublish.Remotes) != 1 || st.LedgerPublish.Remotes[0].Ahead != 0 || len(st.Warnings) != 0 {
		t.Errorf("published: %+v, %v", st.LedgerPublish, st.Warnings)
	}

	bd.federation = []byte(`{"peers":[{"URL":"%URL%","Reachable":false,"ReachError":"fetch from origin: unreachable\nhint: x","Status":{"LocalAhead":2,"LocalBehind":1,"Peer":"origin"}}],"pendingChanges":3}`)
	st, err = intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	r := st.LedgerPublish.Remotes[0]
	if r.Ahead != 2 || r.Behind != 1 || r.Fetched || r.FetchError != "fetch from origin: unreachable" || st.LedgerPublish.Uncommitted != 3 {
		t.Errorf("publish = %+v", st.LedgerPublish)
	}
	joined := strings.Join(st.Warnings, "\n")
	for _, w := range []string{"ledger diverged from Dolt remote origin, as of the last pull or fetch", "3 uncommitted change(s)", "bops sync"} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings %v lack %q", st.Warnings, w)
		}
	}

	bd.federation = []byte(`{"peers":[{"URL":"%URL%","Reachable":true,"Status":{"LocalAhead":1,"LocalBehind":0,"Peer":"origin"}}],"pendingChanges":0}`)
	st, _ = intent.Status(bg, a.env, intent.StatusOpts{})
	if len(st.Warnings) != 1 || !strings.HasPrefix(st.Warnings[0], "ledger unpublished: 1 local commit(s) Dolt remote origin lacks, as of now") {
		t.Errorf("warnings = %v", st.Warnings)
	}

	bd.federation = []byte(`not json`)
	st, _ = intent.Status(bg, a.env, intent.StatusOpts{})
	if st.LedgerPublish != nil || len(st.Warnings) != 1 || !strings.HasPrefix(st.Warnings[0], "ledger publication not read: ") {
		t.Errorf("unread: %+v, %v", st.LedgerPublish, st.Warnings)
	}
}

// bd's -1 (no tracking branch) is unknown, not published; a remote that did
// not answer is said so even with nothing ahead; a mirror bops does not push
// to is left out; and a failed claim-ref fetch skips the read.
func TestStatusPublicationEdges(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	for _, tc := range []struct{ doc, want string }{
		{`{"peers":[{"URL":"%URL%","Reachable":false,"ReachError":"no branch","Status":{"LocalAhead":-1,"LocalBehind":-1,"Peer":"origin"}}]}`, "ledger publication unknown: bd has no tracking branch for Dolt remote origin"},
		{`{"peers":[{"URL":"%URL%","Reachable":false,"ReachError":"timeout","Status":{"LocalAhead":0,"LocalBehind":0,"Peer":"origin"}}]}`, "ledger publication not checked against Dolt remote origin: it did not answer (timeout)"},
		{`{"peers":[{"URL":"git+https://example.invalid/mirror.git","Reachable":true,"Status":{"LocalAhead":9,"LocalBehind":0,"Peer":"mirror"}}]}`, "bd lists no Dolt remote on the lock remote's repository"},
	} {
		bd.federation = []byte(tc.doc)
		st, err := intent.Status(bg, a.env, intent.StatusOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.Contains(w, tc.want) }) || slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.HasPrefix(w, "ledger unpublished") }) {
			t.Errorf("%s: warnings = %v", tc.doc, st.Warnings)
		}
	}
	bd.calls = nil
	a.git.offline = true
	st, err := intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if bd.count("federation") != 0 || st.LedgerPublish != nil ||
		!slices.ContainsFunc(st.Warnings, func(w string) bool {
			return w == "ledger publication not read: the lock remote did not answer the fetch"
		}) {
		t.Errorf("after a failed fetch: %+v, %v", st.LedgerPublish, st.Warnings)
	}
}

// With the lock off a claim still pushes, so status reads no publication.
func TestLockOffStatusReadsNoPublication(t *testing.T) {
	bd := newBD()
	r := lockOffRig(t, "A", bd)
	st, err := intent.Status(bg, r.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if st.LedgerPublish != nil || bd.count("federation") != 0 {
		t.Errorf("%+v, %v", st.LedgerPublish, bd.calls)
	}
}

// A claim's display that reaches the remote only after its claim was taken
// over and handed back on another machine arrives as the merged row the
// pull measurement found: in progress, no assignee, the dead run's run:
// label and the release's bops:hold, updated at the release. With no ref
// for the bead, stale lists it as a ledger claim, and reap releases it.
func TestALateDisplayAfterATakeoverIsReaped(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1") // A claimed; its display is on A's ledger only
	bdB.beads["x-1"].Status, bdB.beads["x-1"].Assignee, bdB.beads["x-1"].Labels = "open", "", nil
	// B never saw A's display, so B's takeover sees no ledger claim.
	mustForce(t, b, "x-1")
	if out, err := intent.Release(bg, b.env, "x-1", "abandoned"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	if sha, _, _ := b.env.Lock.Live(bg, "x-1"); sha != "" {
		t.Fatalf("ref left after the release: %s", sha)
	}
	// A's machine pushes at last; B's next pull merges its display in.
	released := time.Now().UTC()
	n := bdB.beads["x-1"]
	n.Status, n.Assignee, n.Labels, n.Updated = "in_progress", "", []string{"bops:hold", "run:A"}, released
	n.Comments = append([]string{"bops:claimed run=A actor=tester machine=box"}, n.Comments...)
	later(b, 3*time.Hour)

	st, err := intent.Stale(bg, b.env, intent.StaleOpts{AllMachines: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Stale) != 1 || st.Stale[0].ID != "x-1" || st.Stale[0].Source != "ledger" || st.Stale[0].Run != "A" || len(st.Orphans) != 0 {
		t.Fatalf("stale = %+v", st)
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil || reapIDs(out.Released) != "x-1" {
		t.Fatalf("reap: %+v, %v", out, err)
	}
	if n.Status != "open" || slices.Contains(n.Labels, "run:A") {
		t.Errorf("ledger after reap = %+v", n)
	}
}

// A late display that merges in beside a live holder's claim leaves the
// dead run's run: label next to the live one; the live holder's next
// display write (sync's repair here) removes it.
func TestALiveHoldersNextDisplayWriteRemovesADeadRunLabel(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	n := bd.beads["x-1"]
	n.Labels = append(n.Labels, "run:DEAD") // the union a pull leaves
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(n.Labels, []string{"run:A"}) || n.Assignee != "tester" {
		t.Errorf("display after the holder's sync = %+v", n)
	}
}
