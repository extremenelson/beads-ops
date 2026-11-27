package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// reapWorld has A claim each id, then shows B a ledger where those claims are
// A's and three hours old: what B sees of a dead run on another machine.
func reapWorld(t *testing.T, ids ...string) (a, b *rig, bdB *fakeBD) {
	t.Helper()
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	for _, id := range ids {
		bdA.add(id)
		bdB.add(id)
	}
	a, b = client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	for _, id := range ids {
		mustClaim(t, a, id)
		n := bdB.beads[id]
		n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A"}
	}
	later(b, 3*time.Hour)
	return a, b, bdB
}

// pushBranch puts a branch with one commit on A's remote.
func pushBranch(t *testing.T, r *rig, name string) {
	t.Helper()
	git(t, r.dir, "-c", "user.name=t", "-c", "user.email=t@invalid", "commit", "-q", "--allow-empty", "-m", "work")
	git(t, r.dir, "push", "-q", "origin", "HEAD:refs/heads/"+name)
}

func reapIDs(ds []intent.ReapDone) string {
	var s []string
	for _, d := range ds {
		s = append(s, d.ID)
	}
	return strings.Join(s, ",")
}

func TestReapDryRunListsOnlyClaimsWithNoWorkAndWritesNothing(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-idle", "x-branch", "x-mr", "x-land")
	pushBranch(t, a, "fix/some-slug-branch")
	bdB.beads["x-mr"].Comments = []string{"MR: https://gitlab.example.com/g/p/-/merge_requests/12 (abc1234)"}
	// Work left in a worktree on the holder's machine: no branch or MR here.
	bdB.beads["x-land"].Labels = append(bdB.beads["x-land"].Labels, "needs-land")
	bdB.calls = nil

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := reapIDs(out.WouldRelease); got != "x-idle" || len(out.Released) != 0 || !out.DryRun {
		t.Fatalf("would_release = %q, want x-idle: %+v", got, out)
	}
	kept := map[string]string{}
	for _, k := range out.Kept {
		kept[k.ID] = strings.Join(k.Evidence, ";")
	}
	if !strings.Contains(kept["x-branch"], "branch fix/some-slug-branch (remote origin)") {
		t.Errorf("x-branch evidence = %q", kept["x-branch"])
	}
	if !strings.Contains(kept["x-mr"], "mr https://gitlab.example.com/g/p/-/merge_requests/12") {
		t.Errorf("x-mr evidence = %q", kept["x-mr"])
	}
	if kept["x-land"] != "label needs-land" {
		t.Errorf("x-land evidence = %q", kept["x-land"])
	}
	if out.Warnings[0] != intent.ReapNotSeen {
		t.Errorf("warnings = %v, want the other-machine caveat first", out.Warnings)
	}
	for _, id := range []string{"x-idle", "x-branch", "x-mr", "x-land"} {
		if bdB.wrote(id) {
			t.Errorf("dry run wrote %s: %v", id, bdB.calls)
		}
	}
	if sha := liveSHA(t, a, "x-idle"); sha == "" {
		t.Error("dry run removed the claim ref")
	}
}

func TestReapReleasesAnIdleClaimWithANote(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-idle", "x-branch")
	pushBranch(t, a, "work/x-branch")

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-idle" || len(out.Failed) != 0 || len(out.Kept) != 1 || out.Kept[0].ID != "x-branch" {
		t.Fatalf("%+v", out)
	}
	n := bdB.beads["x-idle"]
	if n.Status != "open" || n.Assignee != "" {
		t.Errorf("ledger after reap = %+v", n)
	}
	all := strings.Join(n.Comments, "\n")
	if !strings.Contains(all, "takeover=A") || !strings.Contains(all, "Reaped: the claim by tester (run A)") || !strings.Contains(all, "NEXT:") {
		t.Errorf("comments = %v", n.Comments)
	}
	if sha, _, err := b.env.Lock.Live(bg, "x-idle"); err != nil || sha != "" {
		t.Errorf("claim ref after reap = %q, %v; want gone", sha, err)
	}
	if bdB.beads["x-branch"].Assignee != "tester" || bdB.wrote("x-branch") {
		t.Errorf("kept claim was touched: %+v", bdB.beads["x-branch"])
	}
}

func TestReapKeepsAClaimRefreshedSinceStaleReadIt(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	// A notes between B's stale read and B's takeover: claim --force sees the
	// fresh ref and refuses, so reap reports it and writes nothing.
	// A's clock moves with B's, so the note stamps the ref three hours on.
	later(a, 3*time.Hour)
	a.env.Lock.Now = a.env.Now // the ref's at comes from the lock's clock
	b.git.afterSeen = func() {
		if out, err := intent.Note(bg, a.env, "x-1", "still here\n\nNEXT: finish"); err != nil || out.Outcome != "noted" {
			t.Errorf("note: %+v, %v", out, err)
		}
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// The holder came back: nothing failed, so the run is not negative.
	if len(out.Released) != 0 || len(out.Failed) != 0 || len(out.Skipped) != 1 || out.Skipped[0].Outcome != intent.OutcomeRefused ||
		!strings.Contains(out.Skipped[0].Detail, "changed since this run judged it stale") || out.Negative() {
		t.Fatalf("want the takeover refused and listed under skipped: %+v", out)
	}
	if bdB.beads["x-1"].Assignee != "tester" || bdB.beads["x-1"].Status != "in_progress" {
		t.Errorf("ledger = %+v", bdB.beads["x-1"])
	}
}

func TestReapReleasesNothingWhenBranchesCannotBeListed(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	b.git.offline = true

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err == nil || !strings.Contains(err.Error(), "nothing was released") {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
	if len(out.Released) != 0 || bdB.wrote("x-1") {
		t.Errorf("wrote while blind: %+v %v", out, bdB.calls)
	}
}

func TestReapNeedsTheLock(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1")
	b.env.LockOff = true
	if _, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true}); err == nil {
		t.Fatal("reap ran with the lock off")
	}
}

func TestBranchNamesBead(t *testing.T) {
	for _, tc := range []struct {
		branch, id string
		want       bool
	}{
		{"work/bops-n71", "bops-n71", true},
		{"fix/bead-loop-publish-retry-p5s15", "development-collaboration-p5s15", true},
		{"feat/bead-prep-not-slingable-pass-zzqa16", "development-collaboration-zzqa.16", true},
		{"feat/x-zzqa.16", "development-collaboration-zzqa.16", true},
		{"chore/BOPS-N71-export", "bops-n71", true},
		{"work/bops-n711", "bops-n71", false},
		{"fix/n71-later", "bops-n71", true}, // the suffix as any token
		{"main", "bops-n71", false},
		{"work/abops-n71", "bops-n71", true},    // the suffix as a token: a false match only keeps a claim
		{"work/bops-n71.fix", "bops-n71", true}, // '.' bounds a token
		{"work/bops-n71", "bops-n71.2", true},   // a child's work on its parent's branch
		{"x-det", "x-det", true},                // a worktree directory
		{"wt-sibling", "x-hold", false},
		// A short numeric suffix between dots is a version, not the bead.
		{"dependabot/npm_and_yarn/foo-1.12.0", "bops-12", false},
		{"foo-12.0", "bops-12", true}, // a dot on one side only still counts
		{"fix/12.wip", "bops-12", true},
		{"release-12.1", "bops-12", true},
		{"feat/1.2.wip", "bops-1.2", true}, // a child's dotted part still counts
		{"work/bops-12", "bops-12", true},
		{"fix/12", "bops-12", true}, // bounded otherwise it still counts
		{"12-login", "bops-12", true},
		{"release-12", "bops-12", true}, // indistinguishable from fix-12: kept
		{"work/bops-12.3", "bops-12.3", true},
		{"lib-1.123.0", "bops-123", true}, // three digits count even between dots
		{"v1.a1.0", "bops-a1", true},      // so does a letter
	} {
		if got := intent.BranchNamesBead(tc.branch, tc.id); got != tc.want {
			t.Errorf("BranchNamesBead(%q, %q) = %v, want %v", tc.branch, tc.id, got, tc.want)
		}
	}
}

// commit makes one empty commit in dir.
func commit(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@invalid", "commit", "-q", "--allow-empty", "-m", "work")
}

func keptEvidence(out *intent.ReapOut) map[string]string {
	m := map[string]string{}
	for _, k := range out.Kept {
		m[k.ID] = strings.Join(k.Evidence, ";")
	}
	return m
}

func TestReapSeesLocalBranchesDetachedWorktreesAndLiveHolds(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-idle", "x-loc", "x-det")
	commit(t, b.dir)
	git(t, b.dir, "branch", "fix/never-pushed-loc")
	git(t, b.dir, "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(b.dir), "x-det"))
	// A sibling worktree of B's clone holds x-hold, under a branch and a
	// directory that do not name it: only the clone's hold store knows.
	sib := filepath.Join(filepath.Dir(b.dir), "wt-sibling")
	git(t, b.dir, "worktree", "add", "-q", "-b", "feature/unrelated", sib)
	c := rigAt(t, sib, "C", bdB)
	bdB.add("x-hold")
	mustClaim(t, c, "x-hold") // made now, so three hours old to B
	if err := b.store.Refresh(); err != nil {
		t.Fatal(err)
	}

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := reapIDs(out.WouldRelease); got != "x-idle" {
		t.Fatalf("would_release = %q, want x-idle: %+v", got, out)
	}
	ev := keptEvidence(out)
	for id, want := range map[string]string{
		"x-loc":  "branch fix/never-pushed-loc (local)",
		"x-det":  "(worktree directory ",
		"x-hold": "hold of run C in live worktree ", // the path may be resolved (/private on macOS)
	} {
		if !strings.Contains(ev[id], want) {
			t.Errorf("%s evidence = %q, want it to contain %q", id, ev[id], want)
		}
	}
}

func TestReapKeepsWorkThatArrivesWithTheTakeoversPull(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	pulls := 0
	bdB.onPull = func() { // the second pull is claim --force's
		if pulls++; pulls == 2 {
			n := bdB.beads["x-1"]
			n.Labels = append(n.Labels, "needs-land")
		}
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Released) != 0 || len(out.Failed) != 1 || out.Failed[0].Outcome != intent.OutcomeHeld ||
		!strings.Contains(out.Failed[0].Detail, "found work: label needs-land") ||
		!strings.Contains(out.Failed[0].Detail, "this run now holds x-1") || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if n := bdB.beads["x-1"]; n.Status != "in_progress" || n.Assignee != "tester" {
		t.Errorf("the taken-over bead must stay held, not go back to the pool: %+v", n)
	}
}

func TestReapEvidenceSources(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-gh", "x-landed", "x-notes", "x-desc", "x-ac", "x-bare", "x-pr", "x-bang",
		"x-mr", "x-paren", "x-port", "x-repo", "x-none")
	bdB.beads["x-gh"].Comments = []string{"PR at https://github.com/o/r/pull/7 for review"}
	bdB.beads["x-notes"].Notes = "see https://bitbucket.example.com/p/r/pull-requests/3"
	bdB.beads["x-landed"].Metadata = json.RawMessage(`{"bops":{"landed":[{"ref":"https://github.com/o/r/pull/8","sha":"abc","run":"A","at":"t"}]}}`)
	bdB.beads["x-desc"].Description = "Opened https://gitlab.example.com/g/p/-/merge_requests/9."
	bdB.beads["x-ac"].Acceptance = "Merged as https://github.com/o/r/pull/10"
	// References without a scheme.
	bdB.beads["x-bare"].Notes = "pushed: github.com/o/r/pull/12"
	bdB.beads["x-pr"].Comments = []string{"opened PR #128 upstream"}
	bdB.beads["x-bang"].Comments = []string{"see !45 for the fix"}
	bdB.beads["x-mr"].Comments = []string{"merged as MR !46"}
	bdB.beads["x-paren"].Notes = "landed (!47)"
	bdB.beads["x-port"].Notes = "gitlab.example.com:8443/g/p/-/merge_requests/3"
	bdB.beads["x-repo"].Notes = "opened `g/p!48`"
	bdB.beads["x-none"].Description = "wow!45 is no reference, nor issue #7 or o/r#8, xpr #5, PR\n#6 or v1.2"
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	ev := keptEvidence(out)
	for id, want := range map[string]string{
		"x-gh":     "mr https://github.com/o/r/pull/7",
		"x-landed": "mr https://github.com/o/r/pull/8",
		"x-notes":  "mr https://bitbucket.example.com/p/r/pull-requests/3",
		"x-desc":   "mr https://gitlab.example.com/g/p/-/merge_requests/9",
		"x-ac":     "mr https://github.com/o/r/pull/10",
		"x-bare":   "mr github.com/o/r/pull/12",
		"x-pr":     "mr PR #128",
		"x-bang":   "mr !45",
		"x-mr":     "mr MR !46",
		"x-paren":  "mr !47",
		"x-port":   "mr gitlab.example.com:8443/g/p/-/merge_requests/3",
		"x-repo":   "mr g/p!48",
	} {
		if ev[id] != want {
			t.Errorf("%s evidence = %q, want %q", id, ev[id], want)
		}
	}
	if reapIDs(out.WouldRelease) != "x-none" {
		t.Fatalf("would_release = %q, want x-none: %+v", reapIDs(out.WouldRelease), out)
	}
}

func TestReapLeavesOrphansAndItsOwnHold(t *testing.T) {
	for _, dry := range []bool{true, false} {
		a, b, bdB := reapWorld(t, "x-orphan")
		// B's ledger shows no claim on x-orphan though A's ref holds it.
		n := bdB.beads["x-orphan"]
		n.Status, n.Assignee, n.Labels = "open", "", nil
		bdB.add("x-own")
		later(b, 0)
		mustClaim(t, b, "x-own")
		later(b, 3*time.Hour)

		out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Released) != 0 || len(out.Failed) != 0 || len(out.WouldRelease) != 0 || len(out.Skipped) != 0 {
			t.Fatalf("dry=%v: %+v", dry, out)
		}
		if ev := keptEvidence(out); !strings.Contains(ev["x-own"], "hold of run B") {
			t.Errorf("dry=%v: own hold evidence = %q", dry, ev["x-own"])
		}
		if sha := liveSHA(t, a, "x-orphan"); sha == "" {
			t.Errorf("dry=%v: reap removed an orphan ref", dry)
		}
	}
}

func TestReapReleasesALedgerClaimWithNoRef(t *testing.T) {
	_, b, bdB := reapWorld(t)
	n := bdB.add("x-raw")
	n.Status, n.Assignee, n.Labels = "in_progress", "someone", []string{"run:RAW"}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-raw" || n.Status != "open" || n.Assignee != "" {
		t.Fatalf("%+v; ledger %+v", out, n)
	}
}

// A ledger claim (no ref) pins by its run label, and is matched by any of its
// run labels, whatever order bd lists them in.
func TestReapOnlyPinsALedgerClaim(t *testing.T) {
	_, b, bdB := reapWorld(t)
	n := bdB.add("x-raw")
	n.Status, n.Assignee, n.Labels = "in_progress", "someone", []string{"run:OLD", "run:RAW"}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Only: []string{"x-raw@RAW"}})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-raw" || n.Status != "open" || n.Assignee != "" {
		t.Fatalf("%+v; ledger %+v", out, n)
	}
}

func TestReapRefusesAWindowShorterThanStaleMinutes(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1")
	if _, err := intent.Reap(bg, b.env, intent.ReapOpts{OlderThan: 30 * time.Minute}); !errors.Is(err, intent.ErrPrecondition) {
		t.Fatalf("err = %v, want a precondition refusal", err)
	}
}

func TestReapStartsNoTakeoverWithoutTimeForTheRelease(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1", "x-2")
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	out, err := intent.Reap(ctx, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Skipped) != 2 || len(out.Released) != 0 || !out.Negative() || bdB.wrote("x-1") || bdB.wrote("x-2") {
		t.Fatalf("%+v", out)
	}
}

func TestReapOnlyActsOnTheNamedClaims(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-a", "x-b")
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Only: []string{"x-a", "x-gone"}})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-a" || len(out.Skipped) != 1 || out.Skipped[0].ID != "x-gone" || bdB.wrote("x-b") {
		t.Fatalf("%+v", out)
	}
	if _, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Only: []string{"bad id"}}); err == nil {
		t.Error("a malformed id was accepted")
	}
}

// A claim taken over between the preview and the real run is not the holder
// the operator saw: --only id@run skips it, while a bare id reaps it as before.
func TestReapOnlyPinsTheRunThePreviewShowed(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-pinned", "x-moved", "x-bare")
	prev, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil || reapIDs(prev.WouldRelease) != "x-bare,x-moved,x-pinned" || prev.WouldRelease[0].Run != "A" {
		t.Fatalf("preview: %v, %+v", err, prev)
	}
	// Someone force-took x-moved and x-bare since, then went idle too. A
	// ledger-only claim stands in for the new holder: its run label is the run.
	for _, id := range []string{"x-moved", "x-bare"} {
		git(t, b.dir, "push", "-q", "origin", ":refs/bops/claims/"+id)
		bdB.beads[id].Labels = []string{"run:C"}
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Only: []string{"x-pinned@A", "x-moved@A", "x-bare"}})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-pinned,x-bare" || reapIDs(out.Skipped) != "x-moved" || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if d := out.Skipped[0]; d.Run != "C" || !strings.Contains(d.Detail, "held now by run C, not run A") {
		t.Errorf("skipped = %+v", d)
	}
	if n := bdB.beads["x-moved"]; n.Assignee != "tester" || n.Status != "in_progress" || bdB.wrote("x-moved") {
		t.Errorf("a claim of another run was touched: %+v", n)
	}
}

func TestReapOnlyRejectsBadPins(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1")
	for _, only := range [][]string{{"x-1@"}, {"x-1@A B"}, {"x-1@A@B"}, {"x-1@-"}, {"x-1@A\u200b"}, {"@A"}, {"x-1@A", "x-1@B"}, {"x-1", "x-1@A"}} {
		if _, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Only: only}); err == nil {
			t.Errorf("--only %q was accepted", only)
		}
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Only: []string{"x-1@A", "x-1@A"}})
	if err != nil || reapIDs(out.WouldRelease) != "x-1" {
		t.Fatalf("the same pin twice: %v, %+v", err, out)
	}
}

// The takeover itself re-checks the run, on the read its lease is taken on:
// a claim another run took between stale's read and the takeover is refused,
// pinned or not, whether the new holder has a ref or only the ledger shows it.
// It is listed under skipped, and makes the run negative only when named.
func TestReapTakeoverRefusesAClaimAnotherRunTook(t *testing.T) {
	for _, viaRef := range []bool{false, true} {
		for _, only := range [][]string{nil, {"x-1"}, {"x-1@A"}} {
			_, b, bdB := reapWorld(t, "x-1")
			b.git.afterSeen = func() {
				git(t, b.dir, "push", "-q", "origin", ":refs/bops/claims/x-1")
				bdB.beads["x-1"].Labels = []string{"run:C"}
				if viaRef { // run C claims on its own clock, three hours behind B's: stale to B
					bdC := newBD()
					bdC.add("x-1")
					mustClaim(t, client(t, strings.TrimSpace(git(t, b.dir, "remote", "get-url", "origin")), "C", bdC), "x-1")
				}
			}
			out, err := intent.Reap(bg, b.env, intent.ReapOpts{Only: only})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Released) != 0 || len(out.Failed) != 0 || len(out.Skipped) != 1 || out.Skipped[0].Outcome != intent.OutcomeRefused ||
				!strings.Contains(out.Skipped[0].Detail, "held now by run C, not run A") || out.Negative() != (only != nil) {
				t.Fatalf("ref %v, --only %q: %+v", viaRef, only, out)
			}
			if n := bdB.beads["x-1"]; n.Assignee != "tester" || n.Status != "in_progress" || bdB.wrote("x-1") {
				t.Errorf("ref %v, --only %q: ledger = %+v", viaRef, only, n)
			}
			if viaRef {
				if ref, err := b.env.Lock.Read(bg, "x-1"); err != nil || ref == nil || ref.Run != "C" {
					t.Errorf("--only %q: ref after reap = %+v, %v; want run C's", only, ref, err)
				}
			}
		}
	}
}

// headsFail fails the nth and later `git ls-remote --heads` calls, the
// branch listing, and passes every other git call through.
type headsFail struct {
	lock.Git
	n, calls int
}

func (g *headsFail) Run(ctx context.Context, c lock.Cmd) (lock.Output, error) {
	if len(c.Args) > 1 && c.Args[0] == "ls-remote" && c.Args[1] == "--heads" {
		if g.calls++; g.calls >= g.n {
			return lock.Output{Stderr: "fatal: unable to access remote"}, errors.New("exit status 128")
		}
	}
	return g.Git.Run(ctx, c)
}

func TestReapReleasesNothingWhenThePullFails(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	bdB.fail = func(a []string) error {
		if a[0] == "dolt" && a[1] == "pull" {
			return errors.New("remote down")
		}
		return nil
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err == nil || !strings.Contains(err.Error(), "ledger pull failed") || len(out.Released) != 0 || bdB.wrote("x-1") {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
}

// A preview reads the comments stale did not, such as a claim of this clone's
// own run; one it cannot read keeps the claim too.
func TestReapPreviewKeepsABeadWhoseEvidenceCannotBeRead(t *testing.T) {
	_, b, bdB := reapWorld(t)
	bdB.add("x-own")
	later(b, 0)
	mustClaim(t, b, "x-own")
	later(b, 3*time.Hour)
	bdB.fail = func(a []string) error {
		if a[0] == "comments" && a[1] == "x-own" {
			return errors.New("dolt: database is locked")
		}
		return nil
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if ev := keptEvidence(out)["x-own"]; !strings.HasPrefix(ev, "unread: ") || len(out.WouldRelease) != 0 || len(out.Skipped) != 0 {
		t.Fatalf("evidence %q: %+v", ev, out)
	}
}

// A read the command's deadline cuts short is not evidence: the claim and
// the rest are listed under skipped, to run again, not kept as unread.
func TestReapSkipsWhatTheDeadlineCutShort(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1", "x-2")
	ctx, cancel := context.WithTimeout(bg, time.Hour)
	defer cancel()
	listed := false
	bdB.fail = func(a []string) error {
		if a[0] == "list" {
			listed = true
		} else if listed && a[0] == "show" && a[1] == "x-1" { // the read just before the takeover
			cancel()
			return context.DeadlineExceeded
		}
		return nil
	}
	out, err := intent.Reap(ctx, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Kept) != 0 || reapIDs(out.Skipped) != "x-1,x-2" || !out.Negative() ||
		!strings.Contains(out.Skipped[0].Detail, "run reap again") || bdB.wrote("x-1") {
		t.Fatalf("%+v", out)
	}
}

// So is a preview's forge search cut short by the command's deadline; the
// forge is not called unanswered for it.
func TestReapPreviewSkipsASearchTheDeadlineCutShort(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1", "x-2")
	ctx, cancel := context.WithTimeout(bg, time.Hour)
	defer cancel()
	f := &searchForge{block: true, onSearch: cancel} // the deadline falls during the first search
	out, err := intent.Reap(ctx, b.env, intent.ReapOpts{DryRun: true, Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Kept) != 0 || reapIDs(out.Skipped) != "x-1,x-2" || slices.Contains(out.Warnings, intent.ReapForgeUnanswered) ||
		out.Skipped[0].Detail != "not reached: too little of the command's time left; run reap again" || len(f.asked) != 1 {
		t.Fatalf("%+v (asked %v)", out, f.asked)
	}
}

// Two reapers racing: the loser's takeover finds the claim already released
// (or taken). Nothing was left undone, so it is listed under skipped and the
// run is not negative, unless --only named the claim.
func TestReapLosingARaceToAnotherReaperIsNotAFailure(t *testing.T) {
	for _, only := range [][]string{nil, {"x-1"}} {
		_, b, bdB := reapWorld(t, "x-1")
		pulls := 0
		bdB.onPull = func() { // the other reaper's release lands with the takeover's pull
			if pulls++; pulls == 2 {
				git(t, b.dir, "push", "-q", "origin", ":refs/bops/claims/x-1")
				n := bdB.beads["x-1"]
				n.Status, n.Assignee, n.Labels = "open", "", []string{"bops:hold"}
			}
		}
		out, err := intent.Reap(bg, b.env, intent.ReapOpts{Only: only})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Failed) != 0 || len(out.Released) != 0 || len(out.Skipped) != 1 ||
			!strings.Contains(out.Skipped[0].Detail, "nothing to take over") || out.Negative() != (only != nil) {
			t.Fatalf("--only %q: %+v", only, out)
		}
	}
}

// A ledger claim (no ref) touched between stale's read and the takeover is
// no longer stale: skipped, not a failure. One whose time is unreadable or in
// the future is refused as before, and is a failure.
func TestReapLedgerClaimRefreshedBeforeTheTakeover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updated func(now time.Time) time.Time
		moved   bool
	}{
		{"refreshed", func(now time.Time) time.Time { return now.Add(-time.Minute) }, true},
		{"future", func(now time.Time) time.Time { return now.Add(time.Hour) }, false},
	} {
		_, b, bdB := reapWorld(t)
		n := bdB.add("x-raw")
		n.Status, n.Assignee, n.Labels = "in_progress", "someone", []string{"run:RAW"}
		pulls := 0
		bdB.onPull = func() { // the takeover's pull brings the touch
			if pulls++; pulls == 2 {
				n.Updated = tc.updated(b.env.Now())
			}
		}
		out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if tc.moved && (len(out.Skipped) != 1 || len(out.Failed) != 0 || out.Negative()) ||
			!tc.moved && (len(out.Failed) != 1 || len(out.Skipped) != 0 || !out.Negative()) {
			t.Fatalf("%s: %+v", tc.name, out)
		}
		if n.Status != "in_progress" || n.Assignee != "someone" {
			t.Errorf("%s: ledger = %+v", tc.name, n)
		}
	}
}

func TestReapKeepsABeadWhoseEvidenceCannotBeRead(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	bdB.fail = func(a []string) error {
		if a[0] == "show" && a[1] == "x-1" { // stale reads the list, not show
			return errors.New("dolt: database is locked")
		}
		return nil
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ev := keptEvidence(out)
	if !strings.HasPrefix(ev["x-1"], "unread: ") || len(out.Released) != 0 || bdB.wrote("x-1") {
		t.Fatalf("%+v", out)
	}
}

// reads counts the bd show and bd comments (not comments add) calls on id.
func reads(bd *fakeBD, id string) (show, comments int) {
	for _, c := range bd.calls {
		switch {
		case c[0] == "show" && c[1] == id:
			show++
		case c[0] == "comments" && c[1] == id:
			comments++
		}
	}
	return show, comments
}

func TestReapReusesTheIssueAndCommentsStaleRead(t *testing.T) {
	for _, dry := range []bool{true, false} {
		a, b, bdB := reapWorld(t, "x-1", "x-2")
		pushBranch(t, a, "work/x-1")
		bdB.beads["x-2"].Labels = append(bdB.beads["x-2"].Labels, "needs-land")
		bdB.calls = nil
		out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Kept) != 2 {
			t.Fatalf("dry=%v: %+v", dry, out)
		}
		for _, id := range []string{"x-1", "x-2"} {
			// stale reads the list and, for a claim from another machine,
			// its comments once; the evidence read reuses both.
			if show, comments := reads(bdB, id); show != 0 || comments != 1 {
				t.Errorf("dry=%v %s: %d show and %d comments calls, want 0 and 1", dry, id, show, comments)
			}
		}
	}
}

func TestReapReadsNoCommentsForAClaimItsOwnHoldKeeps(t *testing.T) {
	// A claim of this clone's run: stale reads no comments for it, and its
	// live hold keeps it without them.
	_, b, bdB := reapWorld(t)
	bdB.add("x-own")
	later(b, 0)
	mustClaim(t, b, "x-own")
	later(b, 3*time.Hour)
	bdB.calls = nil
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keptEvidence(out)["x-own"], "hold of run B") {
		t.Fatalf("%+v", out)
	}
	if show, comments := reads(bdB, "x-own"); show != 0 || comments != 0 {
		t.Errorf("x-own: %d show and %d comments calls, want none", show, comments)
	}
}

func TestReapReadsTheBeadAfreshJustBeforeATakeover(t *testing.T) {
	// Work recorded after stale read the ledger, as another process in this
	// clone might: only bd show sees it, and it keeps the claim untaken.
	_, b, bdB := reapWorld(t, "x-1")
	listed := false
	bdB.fail = func(a []string) error {
		if a[0] == "list" {
			listed = true
		} else if listed && a[0] == "comments" && a[1] == "x-1" {
			n := bdB.beads["x-1"]
			n.Notes = "MR: https://gitlab.example.com/g/p/-/merge_requests/7"
		}
		return nil
	}
	bdB.calls = nil
	f := &searchForge{}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keptEvidence(out)["x-1"], "merge_requests/7") || len(out.Released)+len(out.Failed) != 0 {
		t.Fatalf("%+v", out)
	}
	if len(f.asked) != 0 {
		t.Errorf("asked the forge %v for a claim the fresh read keeps", f.asked)
	}
	if bdB.wrote("x-1") {
		t.Errorf("took x-1 over though a fresh read showed its work: %v", bdB.calls)
	}
	if show, _ := reads(bdB, "x-1"); show != 1 {
		t.Errorf("x-1: %d show calls, want the one fresh read before the takeover", show)
	}
}

func TestReapListsBranchesAgainBeforeEachTakeover(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1", "x-2")
	pulls := 0
	bdB.onPull = func() { // the second pull is the first takeover's
		if pulls++; pulls == 2 {
			pushBranch(t, a, "work/x-2")
		}
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-1" || !strings.Contains(keptEvidence(out)["x-2"], "branch work/x-2") {
		t.Fatalf("%+v", out)
	}
}

func TestReapStopsWhenALaterBranchListingFails(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1", "x-2", "x-3")
	// 1: before the loop, 2: after x-1's takeover, 3: before x-2's.
	b.env.Lock.Git = &headsFail{Git: b.env.Lock.Git, n: 3}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err == nil || !strings.Contains(err.Error(), "nothing more was released") {
		t.Fatalf("err = %v", err)
	}
	if reapIDs(out.Released) != "x-1" || reapIDs(out.Skipped) != "x-2,x-3" || bdB.wrote("x-2") || bdB.wrote("x-3") {
		t.Fatalf("%+v", out)
	}
}

func TestReapSaysWhenItHoldsAClaimItCouldNotFinish(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	b.env.Lock.Git = &headsFail{Git: b.env.Lock.Git, n: 2} // the listing after the takeover
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err == nil || len(out.Failed) != 1 || out.Failed[0].Outcome != intent.OutcomeHeld ||
		!strings.Contains(out.Failed[0].Detail, "this run now holds x-1") || !out.Negative() {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
	if n := bdB.beads["x-1"]; n.Status != "in_progress" {
		t.Errorf("the bead must stay held, not go back to the pool: %+v", n)
	}
}

func TestReapKeepsAClaimMadeOutsideBopsUnlessNamed(t *testing.T) {
	_, b, bdB := reapWorld(t)
	n := bdB.add("x-plain")
	n.Status, n.Assignee = "in_progress", "someone" // no run label, no ref
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keptEvidence(out)["x-plain"], "claimed outside bops") || len(out.WouldRelease) != 0 {
		t.Fatalf("%+v", out)
	}
	out, err = intent.Reap(bg, b.env, intent.ReapOpts{}) // a real run, not named
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keptEvidence(out)["x-plain"], "claimed outside bops") || len(out.Released)+len(out.Failed) != 0 ||
		out.Negative() || bdB.wrote("x-plain") {
		t.Fatalf("real run, not named: %+v", out)
	}
	out, err = intent.Reap(bg, b.env, intent.ReapOpts{Only: []string{"x-plain", "x-plain"}})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-plain" || len(out.Failed) != 0 || len(out.Skipped) != 0 {
		t.Fatalf("named, and named twice: %+v", out)
	}
}

func TestReapSeesBranchesOnTheLockRemoteOriginAndForks(t *testing.T) {
	a, b, _ := reapWorld(t, "x-org", "x-fork", "x-idle")
	// The claim refs stay where they are, now under the name lockr; origin is
	// a separate repository holding the work, and a fork was fetched once.
	git(t, b.dir, "remote", "rename", "origin", "lockr")
	b.env.Lock.Remote = "lockr"
	code := bareRemote(t)
	git(t, b.dir, "remote", "add", "origin", code)
	commit(t, a.dir)
	git(t, a.dir, "push", "-q", code, "HEAD:refs/heads/work/x-org")
	fork := bareRemote(t)
	git(t, a.dir, "push", "-q", fork, "HEAD:refs/heads/feat/x-fork")
	git(t, b.dir, "remote", "add", "fork", fork)
	git(t, b.dir, "fetch", "-q", "fork")

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	ev := keptEvidence(out)
	if !strings.Contains(ev["x-org"], "branch work/x-org (remote origin)") ||
		!strings.Contains(ev["x-fork"], "branch feat/x-fork (remote fork (last fetch))") ||
		reapIDs(out.WouldRelease) != "x-idle" {
		t.Fatalf("evidence = %v (%+v)", ev, out)
	}
}

func TestReapPreviewIgnoresTheTimeBudget(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1", "x-2")
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	out, err := intent.Reap(ctx, b.env, intent.ReapOpts{DryRun: true})
	if err != nil || reapIDs(out.WouldRelease) != "x-1,x-2" || len(out.Skipped) != 0 || out.Negative() {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
}

func TestReapIsNegativeWhenANamedClaimIsKept(t *testing.T) {
	a, b, _ := reapWorld(t, "x-1")
	pushBranch(t, a, "work/x-1") // work appeared after the preview
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Only: []string{"x-1"}})
	if err != nil || len(out.Released) != 0 || len(out.Kept) != 1 || !out.Negative() {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
}

func TestReapPreviewOfNamedClaims(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-work")
	pushBranch(t, a, "work/x-work")
	n := bdB.add("x-plain")
	n.Status, n.Assignee = "in_progress", "someone" // outside bops
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Only: []string{"x-work", "x-plain", "x-gone"}})
	if err != nil {
		t.Fatal(err)
	}
	// A preview reports; naming lifts the outside-bops keep, but a kept
	// claim with work does not make a preview negative. A missing id does.
	if reapIDs(out.WouldRelease) != "x-plain" || len(out.Kept) != 1 || out.Kept[0].ID != "x-work" ||
		reapIDs(out.Skipped) != "x-gone" || !out.Negative() || bdB.wrote("x-plain") {
		t.Fatalf("%+v", out)
	}
}

// pushFails makes every `bd dolt push` fail while *down is set, and records
// whether the claim ref was still there each time a push landed.
func pushFails(t *testing.T, bd *fakeBD, a *rig, id string, down *bool, refAtPush *[]string) {
	t.Helper()
	bd.fail = func(args []string) error {
		if args[0] == "dolt" && args[1] == "push" {
			if *down {
				return errors.New("remote down")
			}
			*refAtPush = append(*refAtPush, liveSHA(t, a, id))
		}
		return nil
	}
}

// A reap whose takeover and release pushes both fail must not leave the ref
// gone while the remote ledger still shows the old holder: the ref stays,
// release pending, until a sync's push lands, and only then goes.
func TestReapKeepsTheRefUntilTheReleasePushLands(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	down := true
	var refAtPush []string
	pushFails(t, bdB, a, "x-1", &down, &refAtPush)

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-1" || !strings.Contains(out.Released[0].Detail, "pending: run bops sync") || !out.Negative() ||
		out.Released[0].SavedTo != "" { // the note is in the local ledger; the push carries it
		t.Fatalf("%+v", out)
	}
	if bdB.count("dolt push") < 2 { // the takeover's and the release's
		t.Fatalf("pushes = %d, want the takeover's and the release's tried", bdB.count("dolt push"))
	}
	sha := liveSHA(t, a, "x-1")
	if sha == "" {
		t.Fatal("the claim ref was deleted while the remote ledger may still show the old holder")
	}
	if e, ok := b.store.Entry("x-1"); !ok || !e.ReleasePending || e.SHA != sha {
		t.Fatalf("journal = %+v, %v; want release pending on the live ref", e, ok)
	}
	if n := bdB.beads["x-1"]; n.Status != "open" || n.Assignee != "" {
		t.Errorf("local ledger = %+v, want released", n)
	}

	// A sync that cannot push, or is told not to, keeps the ref too.
	if _, err := intent.Sync(bg, b.env, false); err == nil {
		t.Fatal("sync pushed through a down remote")
	}
	s, err := intent.Sync(bg, b.env, true)
	if err != nil || liveSHA(t, a, "x-1") == "" || len(s.LostClaims) != 0 {
		t.Fatalf("sync --no-push: %+v, %v; ref %q", s, err, liveSHA(t, a, "x-1"))
	}

	down = false
	s, err = intent.Sync(bg, b.env, false)
	if err != nil || len(s.LostClaims) != 0 {
		t.Fatalf("sync: %+v, %v", s, err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("sync pushed the release but left the claim ref")
	}
	if len(refAtPush) == 0 || refAtPush[0] == "" {
		t.Errorf("ref at the first push that landed = %q, want it still there", refAtPush)
	}
	if _, ok := b.store.Entry("x-1"); ok {
		t.Error("hold kept after the release finished")
	}
	if n := bdB.beads["x-1"]; n.Status != "open" || n.Assignee != "" || slices.Contains(n.Labels, "run:B") {
		t.Errorf("sync showed the released claim again: %+v", n)
	}
}

// Another command on the bead finishes the pending release first, as sync does.
func TestReapPendingReleaseIsFinishedByTheNextCommandOnTheBead(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	down := true
	var refAtPush []string
	pushFails(t, bdB, a, "x-1", &down, &refAtPush)
	if out, err := intent.Reap(bg, b.env, intent.ReapOpts{}); err != nil || len(out.Released) != 1 {
		t.Fatalf("%+v, %v", out, err)
	}
	if out, err := intent.Release(bg, b.env, "x-1", "abandoned"); err == nil || out.Outcome != intent.OutcomeError || liveSHA(t, a, "x-1") == "" {
		t.Fatalf("release with the remote down: %+v, %v", out, err)
	}
	down = false
	out, err := intent.Release(bg, b.env, "x-1", "abandoned")
	if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Detail, "had not finished") {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" || len(refAtPush) == 0 || refAtPush[0] == "" {
		t.Errorf("ref %q; ref at push %q", liveSHA(t, a, "x-1"), refAtPush)
	}
}

// A release whose local ledger write fails has not happened: reap keeps the
// claim and says so, and the ref stays with it.
func TestReapKeepsTheClaimWhenTheReleaseCannotWriteTheLedger(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	bdB.fail = func(args []string) error {
		if args[0] == "comments" && args[1] == "add" && args[2] == "x-1" && slices.Contains(args, "-f") {
			return errors.New("dolt: database is locked")
		}
		return nil
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err == nil || len(out.Failed) != 1 || !strings.Contains(out.Failed[0].Detail, "this run now holds x-1") || out.Failed[0].SavedTo == "" {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
	if liveSHA(t, a, "x-1") == "" {
		t.Fatal("ref deleted by a release that did not happen")
	}
	if e, ok := b.store.Entry("x-1"); !ok || e.ReleasePending || !e.LedgerPending {
		t.Fatalf("journal = %+v, %v; want the claim kept, ledger pending", e, ok)
	}
}

// Takeover, evidence re-read and release run under one hold of the bead's
// lock: no sync or other bops process in the clone can act between them.
func TestReapHoldsTheBeadLockFromTakeoverToRelease(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	var free []string
	bdB.fail = func(args []string) error {
		n := bdB.beads["x-1"]
		if !slices.Contains(n.Labels, "run:B") && !strings.Contains(strings.Join(n.Comments, "\n"), "run=B reason=abandoned") {
			return nil // before the takeover's display, or after the release's
		}
		if unlock, err := b.store.LockBead("x-1"); err == nil {
			unlock()
			free = append(free, strings.Join(args, " "))
		}
		return nil
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil || reapIDs(out.Released) != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(free) > 0 {
		t.Errorf("bead lock free between takeover and release, at: %v", free)
	}
}

// reap judges claims from the ledger claims write: a Dolt remote on another
// repository is refused before the pull, as claim refuses it.
func TestReapRefusesALedgerOnAnotherRepository(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	bdB.remotes = `[{"name":"origin","url":"git+https://example.invalid/other/repo.git"}]`
	bdB.calls = nil
	if _, err := intent.Reap(bg, b.env, intent.ReapOpts{}); err == nil {
		t.Fatal("reap ran against a ledger on another repository")
	}
	if bdB.count("dolt pull") != 0 || bdB.wrote("x-1") || liveSHA(t, a, "x-1") == "" {
		t.Errorf("reap pulled or wrote: %v", bdB.calls)
	}
}

// When the pushes land, the release's push still comes before the ref delete.
func TestReapPushesTheReleaseBeforeDeletingTheRef(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	down := false
	var refAtPush []string
	pushFails(t, bdB, a, "x-1", &down, &refAtPush)
	inner := bdB.fail
	marked := false // the release's push runs with release_pending already written
	bdB.fail = func(args []string) error {
		if args[0] == "dolt" && args[1] == "push" && strings.Contains(strings.Join(bdB.beads["x-1"].Comments, "\n"), "reason=abandoned") {
			e, _ := b.store.Entry("x-1")
			marked = e.ReleasePending
		}
		return inner(args)
	}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if !marked {
		t.Error("release_pending not written before the release's push: a crash during it would leave a live claim")
	}
	if err != nil || reapIDs(out.Released) != "x-1" || strings.Contains(out.Released[0].Detail, "pending") || out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(refAtPush) < 2 || slices.Contains(refAtPush, "") {
		t.Fatalf("ref at each push = %q, want live at the takeover's and the release's", refAtPush)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("ref left after a release whose push landed")
	}
}

// A hold released but waiting on its push is no claim of the run's: not
// listed as mine, and no reason for claim --next to refuse.
func TestAPendingReleaseIsNotAClaimOfTheRun(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	down := true
	var refAtPush []string
	pushFails(t, bdB, a, "x-1", &down, &refAtPush)
	if out, err := intent.Reap(bg, b.env, intent.ReapOpts{}); err != nil || len(out.Released) != 1 {
		t.Fatalf("%+v, %v", out, err)
	}
	ss, err := intent.SessionStart(bg, b.env)
	if err != nil || slices.Contains(ss.Mine, "x-1") {
		t.Fatalf("session-start: mine = %v, %v", ss.Mine, err)
	}
	cn, err := intent.ClaimNext(bg, b.env)
	if cn != nil && strings.Contains(cn.Reason, "already holds") {
		t.Fatalf("claim --next refused for a released bead: %+v, %v", cn, err)
	}
}

// A pending release whose ref another machine took over meanwhile is that
// machine's claim now: finishing it drops the hold and pushes nothing.
func TestAPendingReleaseTakenOverIsDroppedNotPushed(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1")
	down := true
	var refAtPush []string
	pushFails(t, bdB, a, "x-1", &down, &refAtPush)
	if out, err := intent.Reap(bg, b.env, intent.ReapOpts{}); err != nil || len(out.Released) != 1 {
		t.Fatalf("%+v, %v", out, err)
	}
	takeOver(t, a, "x-1")
	newSHA := liveSHA(t, a, "x-1")
	down = false
	bdB.calls = nil
	if _, err := intent.Sync(bg, b.env, true); err != nil { // --no-push: pushes nothing itself
		t.Fatal(err)
	}
	if rel, err := intent.Release(bg, b.env, "x-1", "abandoned"); err != nil || rel.Outcome != intent.OutcomeLost {
		t.Fatalf("release: %+v, %v; want lost", rel, err)
	}
	if bdB.count("dolt push") != 0 {
		t.Errorf("pushed a moot release over the new holder's claim: %v", bdB.calls)
	}
	if liveSHA(t, a, "x-1") != newSHA {
		t.Error("the new holder's ref was touched")
	}
	if _, ok := b.store.Entry("x-1"); ok {
		t.Error("hold kept after its ref was taken over")
	}
}

// One failed push in a sync is not retried for every other pending release.
func TestSyncTriesOnePendingReleasePushPerRun(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-1", "x-2")
	down := true
	var refAtPush []string
	pushFails(t, bdB, a, "x-1", &down, &refAtPush)
	if out, err := intent.Reap(bg, b.env, intent.ReapOpts{}); err != nil || len(out.Released) != 2 {
		t.Fatalf("%+v, %v", out, err)
	}
	bdB.calls = nil
	s, err := intent.Sync(bg, b.env, false)
	if err == nil {
		t.Fatal("sync pushed through a down remote")
	}
	// One pending release's push (5 tries) and sync's own (5 tries); the
	// second pending release is not tried, only warned about.
	if pushes := bdB.count("dolt push"); pushes != 10 || liveSHA(t, a, "x-1") == "" || liveSHA(t, a, "x-2") == "" {
		t.Fatalf("pushes %d, refs %q %q", pushes, liveSHA(t, a, "x-1"), liveSHA(t, a, "x-2"))
	}
	if !strings.Contains(strings.Join(s.Warnings, "\n"), "failed earlier in this sync") {
		t.Errorf("warnings = %v", s.Warnings)
	}
}
