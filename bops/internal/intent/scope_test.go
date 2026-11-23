package intent_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// sibling claims id from a second worktree of a's clone, as a /bead-loop
// orchestrator's run does, and returns that run's rig and its worktree.
func sibling(t *testing.T, a *rig, id string) (*rig, string) {
	t.Helper()
	git(t, a.dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.dir, "worktree", "add", "-q", wt, "-b", "wt")
	s := rigAt(t, wt, "SIB", a.bd)
	mustClaim(t, s, id)
	return s, wt
}

// session-start runs on every session start in any checkout, so it writes
// nothing for a hold another run owns: no display repair, no heartbeat, even
// over a status a person set (bops-yr2). A stranded one is only listed.
func TestSessionStartLeavesAnotherRunsHoldAlone(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	_, wt := sibling(t, a, "x-1")
	n.Status, n.Assignee = "deferred", "" // a person parked it; the display drifted

	main := rigAt(t, a.dir, "A", bd)
	bd.calls = nil
	out, err := intent.SessionStart(bg, main.env)
	if err != nil || len(out.LostClaims) != 0 || len(out.StrandedHolds) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") || bd.count("heartbeat") != 0 {
		t.Errorf("session-start touched another run's hold: %v", bd.calls)
	}
	if n.Status != "deferred" {
		t.Errorf("status = %q, want deferred kept", n.Status)
	}

	git(t, a.dir, "worktree", "remove", "--force", wt)
	main = rigAt(t, a.dir, "A", bd)
	bd.calls = nil
	out, err = intent.SessionStart(bg, main.env)
	if err != nil || strings.Join(out.StrandedHolds, ",") != "x-1" {
		t.Fatalf("a stranded hold is still listed: %+v, %v", out, err)
	}
	if bd.wrote("x-1") || bd.count("heartbeat") != 0 {
		t.Errorf("session-start wrote for a stranded hold: %v", bd.calls)
	}
	if h := main.store.Holds(); len(h) != 1 || h[0].ID != "x-1" {
		t.Errorf("the stranded hold must stay for sync or release to settle: %+v", h)
	}
}

// sync settles every hold in the clone against its ref, but re-displays and
// heartbeats only its own run's: a heartbeat from here would hide another
// run dying (bops-yr2). A lost claim is still reported, whoever held it.
func TestSyncRepairsOnlyItsOwnRunsDisplay(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	sibling(t, a, "x-1")
	n.Assignee = "other"

	main := rigAt(t, a.dir, "A", bd)
	bd.calls = nil
	out, err := intent.Sync(bg, main.env, true)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") || bd.count("heartbeat") != 0 {
		t.Errorf("sync re-displayed or heartbeat another run's live hold: %v", bd.calls)
	}

	git(t, a.dir, "push", "-q", "origin", ":refs/bops/claims/x-1") // someone deleted the claim
	main = rigAt(t, a.dir, "A", bd)
	out, err = intent.Sync(bg, main.env, true)
	if err != nil || !slices.Contains(out.LostClaims, "x-1") {
		t.Fatalf("another run's lost claim is still reported: %+v, %v", out, err)
	}
}

// A person who parks a held bead keeps that status through sync; only who
// holds it is repaired (bops-yr2).
func TestSyncKeepsAParkedStatusOnAHeldBead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")

	for _, parked := range []string{"blocked", "deferred"} {
		n.Status, n.Assignee = parked, "other"
		bd.calls = nil
		if _, err := intent.Sync(bg, a.env, true); err != nil {
			t.Fatal(err)
		}
		if n.Status != parked || n.Assignee != "tester" || !slices.Contains(n.Labels, "run:A") {
			t.Errorf("%s: ledger = %+v, want %s kept and the holder repaired", parked, n, parked)
		}
		if bd.count("update x-1 --assignee tester") != 1 || bd.count("update x-1 --assignee tester --status") != 0 {
			t.Errorf("%s: calls = %v, want one assignee-only update", parked, bd.calls)
		}
	}
}
