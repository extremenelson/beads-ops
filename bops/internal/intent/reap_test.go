package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
	if len(out.Released) != 0 || len(out.Failed) != 1 || out.Failed[0].Outcome != intent.OutcomeRefused {
		t.Fatalf("want the takeover refused and reported: %+v", out)
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
	_, b, bdB := reapWorld(t, "x-gh", "x-landed", "x-notes")
	bdB.beads["x-gh"].Comments = []string{"PR at https://github.com/o/r/pull/7 for review"}
	bdB.beads["x-notes"].Notes = "see https://bitbucket.example.com/p/r/pull-requests/3"
	bdB.beads["x-landed"].Metadata = json.RawMessage(`{"bops":{"landed":[{"ref":"https://github.com/o/r/pull/8","sha":"abc","run":"A","at":"t"}]}}`)
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	ev := keptEvidence(out)
	if ev["x-gh"] != "mr https://github.com/o/r/pull/7" || ev["x-landed"] != "mr https://github.com/o/r/pull/8" ||
		ev["x-notes"] != "mr https://bitbucket.example.com/p/r/pull-requests/3" || len(out.WouldRelease) != 0 {
		t.Fatalf("evidence = %v (%+v)", ev, out)
	}
}

func TestReapLeavesOrphansAndItsOwnHold(t *testing.T) {
	a, b, bdB := reapWorld(t, "x-orphan")
	// B's ledger shows no claim on x-orphan though A's ref holds it.
	n := bdB.beads["x-orphan"]
	n.Status, n.Assignee, n.Labels = "open", "", nil
	bdB.add("x-own")
	later(b, 0)
	mustClaim(t, b, "x-own")
	later(b, 3*time.Hour)

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Released) != 0 || len(out.Failed) != 0 || len(out.WouldRelease) != 0 {
		t.Fatalf("%+v", out)
	}
	if ev := keptEvidence(out); !strings.Contains(ev["x-own"], "hold of run B") {
		t.Errorf("own hold evidence = %q", ev["x-own"])
	}
	if sha := liveSHA(t, a, "x-orphan"); sha == "" {
		t.Error("reap removed an orphan ref")
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
