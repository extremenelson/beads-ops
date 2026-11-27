package intent_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// noLedgerWrites fails if bd was asked to change anything, a heartbeat included.
func noLedgerWrites(t *testing.T, bd *fakeBD) {
	t.Helper()
	for _, c := range bd.calls {
		switch {
		case c[0] == "update", c[0] == "label", c[0] == "heartbeat", c[0] == "close",
			c[0] == "comments" && len(c) > 1 && c[1] == "add",
			c[0] == "dolt" && len(c) > 1 && c[1] == "push":
			t.Errorf("renew wrote the ledger: bd %v", c)
		}
	}
}

func renewed(t *testing.T, out *intent.RenewOut, want map[string]string) {
	t.Helper()
	if len(out.Beads) != len(want) {
		t.Fatalf("beads = %+v, want %v", out.Beads, want)
	}
	for _, b := range out.Beads {
		if want[b.ID] != b.Outcome {
			t.Errorf("%s: %+v, want %s", b.ID, b, want[b.ID])
		}
	}
}

func TestRenewRefreshesTheRefAndWritesNoLedger(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	mustClaim(t, a, "x-2")
	before := map[string]string{"x-1": liveSHA(t, a, "x-1"), "x-2": liveSHA(t, a, "x-2")}
	comments := len(bd.beads["x-1"].Comments)
	bd.calls = nil

	out, err := intent.Renew(bg, a.env, []string{"x-1", "x-2", "x-1"})
	if err != nil || out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeRenewed, "x-2": intent.OutcomeRenewed})
	for id, old := range before {
		after := liveSHA(t, a, id)
		if after == old {
			t.Errorf("%s: the ref should change", id)
		}
		if e, _ := a.store.Entry(id); e.SHA != after {
			t.Errorf("%s: journal = %+v, ref %s", id, e, after)
		}
	}
	noLedgerWrites(t, bd)
	if len(bd.beads["x-1"].Comments) != comments {
		t.Error("renew must not comment")
	}
}

func TestRenewResetsTheStaleAge(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	now := time.Now().UTC()
	a.env.Lock.Now = func() time.Time { return now.Add(-3 * time.Hour) }
	mustClaim(t, a, "x-1")
	bd.beads["x-1"].Updated = now.Add(-3 * time.Hour) // the ref, not updated_at, decides

	st, err := intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: 2 * time.Hour, AllMachines: true})
	if err != nil || len(st.Stale) != 1 || st.Stale[0].AgeMinutes < 179 {
		t.Fatalf("before renew: %+v, %v", st, err)
	}
	a.env.Lock.Now = func() time.Time { return now }
	if out, err := intent.Renew(bg, a.env, []string{"x-1"}); err != nil || out.Beads[0].Outcome != intent.OutcomeRenewed {
		t.Fatalf("%+v, %v", out, err)
	}
	st, err = intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: time.Minute, AllMachines: true})
	if err != nil || len(st.Stale) != 0 {
		t.Fatalf("after renew the claim is fresh: %+v, %v", st, err)
	}
}

func TestRenewRefusesABeadThisRunDoesNotHold(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.calls = nil
	out, err := intent.Renew(bg, a.env, []string{"x-2", "x-1"})
	if err != nil || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-2": intent.OutcomeRefused, "x-1": intent.OutcomeRenewed})
	noLedgerWrites(t, bd)
}

func TestRenewOfADisplacedHoldIsLost(t *testing.T) {
	a, b, bdA, _ := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	takeOver(t, b, "x-1")
	bdA.calls = nil
	out, err := intent.Renew(bg, a.env, []string{"x-1"})
	if err != nil || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeLost})
	if h := out.Beads[0].Holder; h == nil || h.Run != "B" {
		t.Errorf("holder = %+v", h)
	}
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("the hold should be dropped")
	}
	noLedgerWrites(t, bdA)
}

func TestRenewOfflineKeepsTheHoldAndFails(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	a.git.offline = true
	out, err := intent.Renew(bg, a.env, []string{"x-1"})
	if err == nil || lock.CodeOf(err) != lock.CodeUnreachable {
		t.Fatalf("an unrenewed claim is an error the caller retries: %+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeUnreachable})
	if _, ok := a.store.Entry("x-1"); !ok {
		t.Error("an offline renew must keep the claim")
	}
}

func TestRenewRefusesLockOffAndBadIDs(t *testing.T) {
	bd := newBD()
	bd.add("x-1")
	r := lockOffRig(t, "A", bd)
	if _, err := intent.Renew(bg, r.env, []string{"x-1"}); !errors.Is(err, intent.ErrPrecondition) {
		t.Errorf("lock off: %v", err)
	}
	if _, err := intent.Renew(bg, r.env, nil); !errors.Is(err, intent.ErrPrecondition) {
		t.Errorf("no ids: %v", err)
	}
	noLedgerWrites(t, bd)
}

// One bad id in an orchestrator's list must not stop the others' heartbeat.
func TestRenewRefusesABadIDAndRenewsTheRest(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	out, err := intent.Renew(bg, a.env, []string{"../bad", "x-1"})
	if err != nil || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"../bad": intent.OutcomeRefused, "x-1": intent.OutcomeRenewed})
	if out.Beads[0].Code != lock.CodeBadID {
		t.Errorf("code = %q", out.Beads[0].Code)
	}
}

// Another bops process on this machine has x-1: it keeps its hold and ref, the
// run is ref_contention for a retry, and x-2 is still renewed.
func TestRenewOfABusyBeadFailsAndRenewsTheRest(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	mustClaim(t, a, "x-2")
	busy, old := liveSHA(t, a, "x-1"), liveSHA(t, a, "x-2")
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	entry, _ := openStore(t, a.dir, "A").Entry("x-1")
	r := rigAt(t, a.dir, "A", bd)
	bd.calls = nil
	out, err := intent.Renew(bg, r.env, []string{"x-1", "x-2"})
	if lock.CodeOf(err) != lock.CodeContention {
		t.Fatalf("want ref_contention: %+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeError, "x-2": intent.OutcomeRenewed})
	if liveSHA(t, r, "x-1") != busy {
		t.Error("the busy bead's ref must stay")
	}
	if e, ok := openStore(t, r.dir, "A").Entry("x-1"); !ok || e != entry {
		t.Errorf("the busy bead's entry = %+v, %v", e, ok)
	}
	if liveSHA(t, r, "x-2") == old {
		t.Error("x-2 should be renewed")
	}
	noLedgerWrites(t, bd)
}

// Offline, the first bead finds the remote down and the rest are not tried.
func TestRenewOfflineStopsAtTheFirstUnreachableBead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	mustClaim(t, a, "x-2")
	a.git.offline = true
	out, err := intent.Renew(bg, a.env, []string{"x-1", "x-2"})
	if lock.CodeOf(err) != lock.CodeUnreachable {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeUnreachable, "x-2": intent.OutcomeUnreachable})
	if b := out.Beads[1]; b.Code != lock.CodeUnreachable || !strings.Contains(b.Reason, "not tried") {
		t.Errorf("x-2 = %+v", b)
	}
	for _, id := range []string{"x-1", "x-2"} {
		if _, ok := a.store.Entry(id); !ok {
			t.Errorf("%s: the hold must be kept", id)
		}
	}
}

// A hold from before the claim lock is still held, but has no ref to renew.
func TestRenewRefusesALegacyHoldAndKeepsIt(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	writeEntry(t, a.dir, "A", "x-1", runstate.Entry{Phase: runstate.PhaseHeld})
	r := rigAt(t, a.dir, "A", bd)
	out, err := intent.Renew(bg, r.env, []string{"x-1"})
	if err != nil || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeRefused})
	if out.Beads[0].Code != intent.CodeLegacyHold {
		t.Errorf("code = %q", out.Beads[0].Code)
	}
	if e, ok := openStore(t, r.dir, "A").Entry("x-1"); !ok || !e.Legacy() {
		t.Errorf("the legacy hold must be kept: %+v, %v", e, ok)
	}
	noLedgerWrites(t, bd)
}

// A release whose ref delete never finished is finished, not renewed.
func TestRenewFinishesAPendingReleaseInsteadOfRenewing(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	e, _ := openStore(t, a.dir, "A").Entry("x-1")
	e.DeletePending = true
	writeEntry(t, a.dir, "A", "x-1", e)
	r := rigAt(t, a.dir, "A", bd)
	out, err := intent.Renew(bg, r.env, []string{"x-1"})
	if err != nil {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeRefused})
	if liveSHA(t, r, "x-1") != "" {
		t.Error("the release's delete must be finished, and no ref recreated")
	}
	if _, ok := openStore(t, r.dir, "A").Entry("x-1"); ok {
		t.Error("the hold must be gone")
	}
}

// The ref moves between renew's read and its leased push: lost, hold dropped.
func TestRenewLosesARefTakenDuringItsPush(t *testing.T) {
	a, b, bdA, _ := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	a.git.beforePush = func() { takeOver(t, b, "x-1") }
	bdA.calls = nil
	out, err := intent.Renew(bg, a.env, []string{"x-1"})
	if err != nil || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	renewed(t, out, map[string]string{"x-1": intent.OutcomeLost})
	if h := out.Beads[0].Holder; h == nil || h.Run != "B" {
		t.Errorf("holder = %+v", h)
	}
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("the hold should be dropped")
	}
	noLedgerWrites(t, bdA)
}

// A refresh whose reply was lost must not make the next renew read as lost.
func TestRenewAfterALostReplyKeepsTheClaim(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	other := &lock.Lock{Git: a.git, Journal: newMemJournal(), Run: "A", Actor: "tester"}
	res, err := other.Replace(bg, "x-1", liveSHA(t, a, "x-1"), lock.StateHeld)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Intend("x-1", res.SHA); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Renew(bg, a.env, []string{"x-1"})
	if err != nil || out.Beads[0].Outcome != intent.OutcomeRenewed {
		t.Fatalf("%+v, %v", out, err)
	}
}
