package intent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// gcWorld builds one ledger and remote, with claims made by A and C, and
// returns G, a clone that holds nothing, to run gc from:
//
//	x-closed    A, held ref 30m old, then plain bd close     delete (closed)
//	x-landed    A, landing ref 30m old, then plain bd close  delete (closed)
//	x-orphan    C, ref 3h old, ledger open              delete (orphan)
//	x-gone      C, ref 3h old, bead not on the ledger   delete (orphan)
//	x-live      A, claimed and in progress              untouched
//	x-landing   A, landing ref, still in progress       untouched: sweep's
//	x-young     C, ref now, ledger open                 untouched: maybe pending
//	x-reopened  C, ref now, bead closed before it       kept: a pending claim
func gcWorld(t *testing.T) (a, c, g *rig, bd *fakeBD) {
	t.Helper()
	remote := bareRemote(t)
	bd = newBD()
	now := time.Now().UTC()
	for _, id := range []string{"x-closed", "x-landed", "x-orphan", "x-gone", "x-live", "x-landing", "x-young", "x-reopened"} {
		bd.add(id)
	}
	a = client(t, remote, "A", bd)
	c = client(t, remote, "C", bd)
	g = client(t, remote, "G", bd)
	closeRaw := func(id string) { // what plain `bd close` leaves
		n := bd.beads[id]
		n.Status, n.Updated = "closed", now
	}
	mustClaim(t, a, "x-live")
	a.env.Lock.Now = func() time.Time { return now.Add(-30 * time.Minute) } // worked half an hour, then closed
	mustClaim(t, a, "x-closed")
	closeRaw("x-closed")
	for _, id := range []string{"x-landed", "x-landing"} {
		mustClaim(t, a, id)
		markLanded(bd.beads[id])
		if out, err := intent.Release(bg, a.env, id, "done"); err != nil || out.Outcome != intent.OutcomeReleased {
			t.Fatalf("release done: %+v, %v", out, err)
		}
	}
	closeRaw("x-landed")
	a.env.Lock.Now = nil
	c.env.Lock.Now = func() time.Time { return now.Add(-3 * time.Hour) }
	mustClaim(t, c, "x-orphan")
	mustClaim(t, c, "x-gone")
	c.env.Lock.Now = nil
	mustClaim(t, c, "x-young")
	mustClaim(t, c, "x-reopened")
	for _, id := range []string{"x-orphan", "x-young"} {
		n := bd.beads[id]
		n.Status, n.Assignee, n.Labels = "open", "", nil // the ledger write never landed
	}
	bd.beads["x-reopened"].Status = "closed" // Updated stays at the fixture's old date
	delete(bd.beads, "x-gone")
	bd.fail = func(args []string) error {
		if args[0] == "show" && args[1] == "x-gone" {
			// What bdx returns for bd's --json error on an unknown id.
			return errors.New("bd show x-gone --json: exit status 1: no issues found matching the provided IDs (some IDs may reference deleted/purged records)")
		}
		return nil
	}
	return a, c, g, bd
}

func gcIDs(gs []intent.GCRef) string {
	var s []string
	for _, x := range gs {
		s = append(s, x.ID)
	}
	return strings.Join(s, ",")
}

func TestGCDryRunListsClosedAndOrphanRefsAndWritesNothing(t *testing.T) {
	a, _, g, bd := gcWorld(t)
	bd.calls = nil
	out, err := intent.GC(bg, g.env, intent.GCOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := gcIDs(out.WouldDelete); got != "x-closed,x-gone,x-landed,x-orphan" || len(out.Deleted) != 0 || !out.DryRun || out.Negative() {
		t.Fatalf("would_delete = %q: %+v", got, out)
	}
	why := map[string]string{}
	for _, x := range out.WouldDelete {
		why[x.ID] = x.Why
	}
	if why["x-closed"] != intent.GCClosed || why["x-landed"] != intent.GCClosed || why["x-orphan"] != intent.GCOrphan || why["x-gone"] != intent.GCOrphan {
		t.Errorf("why = %v", why)
	}
	if gcIDs(out.Kept) != "x-reopened" || !strings.Contains(out.Kept[0].Detail, "pending") {
		t.Errorf("kept = %+v", out.Kept)
	}
	for _, c := range bd.calls {
		if c[0] == "dolt" || c[0] == "update" || c[0] == "label" || c[0] == "close" {
			t.Errorf("dry run ran bd %v", c)
		}
	}
	for _, id := range []string{"x-closed", "x-landed", "x-orphan", "x-gone"} {
		if liveSHA(t, a, id) == "" {
			t.Errorf("dry run removed %s's ref", id)
		}
	}
}

// The motivating case: beads closed with plain bd close leave their refs, and
// a real run removes them, after which stale lists no orphans.
func TestGCDeletesRefsOfBeadsClosedWithPlainBdClose(t *testing.T) {
	a, _, g, bd := gcWorld(t)
	bd.calls = nil
	out, err := intent.GC(bg, g.env, intent.GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := gcIDs(out.Deleted); got != "x-closed,x-gone,x-landed,x-orphan" || len(out.Failed) != 0 || out.Negative() {
		t.Fatalf("deleted = %q: %+v", got, out)
	}
	if bd.count("dolt pull") != 1 {
		t.Errorf("a real run must pull the ledger first: %v", bd.calls)
	}
	for _, id := range []string{"x-closed", "x-landed", "x-orphan", "x-gone"} {
		if liveSHA(t, a, id) != "" {
			t.Errorf("%s's ref survived gc", id)
		}
	}
	for _, id := range []string{"x-live", "x-landing", "x-young", "x-reopened"} {
		if liveSHA(t, a, id) == "" {
			t.Errorf("gc deleted %s's ref", id)
		}
	}
	st, err := intent.Stale(bg, g.env, intent.StaleOpts{AllMachines: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Orphans) != 0 {
		t.Errorf("stale after gc lists orphans: %+v", st.Orphans)
	}
	if bd.beads["x-closed"].Status != "closed" || bd.wrote("x-closed") {
		t.Errorf("gc touched the ledger: %v", bd.calls)
	}
}

// A ref is re-judged under the bead's lock: an orphan claimed meanwhile stays.
func TestGCKeepsARefWhoseBeadWasClaimedSinceItWasRead(t *testing.T) {
	_, c, g, bd := gcWorld(t)
	shows := 0
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "show" && args[1] == "x-orphan" {
			if shows++; shows == 2 { // the re-read under the lock
				n := bd.beads["x-orphan"]
				n.Status, n.Assignee, n.Labels = "in_progress", "tester", []string{"run:C"}
			}
		}
		return prev(args)
	}
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Kept) != "x-orphan" || len(out.Deleted) != 0 || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if liveSHA(t, c, "x-orphan") == "" {
		t.Error("gc deleted a ref claimed since it read it")
	}
}

// The delete carries a lease on the sha gc read, so a ref re-taken meanwhile
// is left with its new holder.
func TestGCLeavesARefRetakenMeanwhile(t *testing.T) {
	_, c, g, _ := gcWorld(t)
	var taken string
	g.git.beforePush = func() {
		takeOver(t, c, "x-orphan")
		taken = liveSHA(t, c, "x-orphan")
	}
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Failed) != "x-orphan" || !strings.Contains(out.Failed[0].Detail, "changed since gc read it") || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if got := liveSHA(t, c, "x-orphan"); got == "" || got != taken {
		t.Errorf("ref = %q, want the new holder's %q", got, taken)
	}
}

// A closed bead this clone still holds is the holder's to release.
func TestGCKeepsARefThisCloneHolds(t *testing.T) {
	a, _, _, _ := gcWorld(t)
	out, err := intent.GC(bg, a.env, intent.GCOpts{Only: []string{"x-closed"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Kept) != "x-closed" || !strings.Contains(out.Kept[0].Detail, "held by run A") || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if liveSHA(t, a, "x-closed") == "" {
		t.Error("gc deleted a ref this clone holds")
	}
}

func TestGCOnlySkipsIDsThatAreNotGarbage(t *testing.T) {
	a, _, g, _ := gcWorld(t)
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-live", "x-none", "x-closed"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Deleted) != "x-closed" || gcIDs(out.Skipped) != "x-live" || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if liveSHA(t, a, "x-live") == "" || liveSHA(t, a, "x-orphan") == "" {
		t.Error("gc deleted a ref --only did not name")
	}
	if _, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"bad/id"}}); err == nil {
		t.Error("an invalid --only id was accepted")
	}
}

func TestGCDeletesNothingWhenThePullFailsOrTheLockIsOff(t *testing.T) {
	a, _, g, bd := gcWorld(t)
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "dolt" && args[1] == "pull" {
			return errors.New("pull failed")
		}
		return prev(args)
	}
	if _, err := intent.GC(bg, g.env, intent.GCOpts{}); err == nil || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("pull failure: err = %v", err)
	}
	bd.fail = prev
	g.git.offline = true
	if _, err := intent.GC(bg, g.env, intent.GCOpts{}); err == nil || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("fetch failure: err = %v", err)
	}
	g.git.offline = false
	if liveSHA(t, a, "x-orphan") == "" || liveSHA(t, a, "x-closed") == "" {
		t.Error("a failed run deleted a ref")
	}
	g.env.LockOff = true
	if _, err := intent.GC(bg, g.env, intent.GCOpts{DryRun: true}); !errors.Is(err, intent.ErrPrecondition) {
		t.Errorf("lock off: err = %v", err)
	}
}

// Only bd's own wording means a bead is missing: an error that merely says
// "not found", such as bd missing from PATH, leaves the ref, unjudged.
func TestGCKeepsAnOrphanWhoseBeadCannotBeRead(t *testing.T) {
	_, _, g, bd := gcWorld(t)
	for _, failure := range []error{bdx.ErrNotFound, errors.New("bd show x-gone --json: exit status 1: database not found")} {
		bd.fail = func(args []string) error {
			if args[0] == "show" && args[1] == "x-gone" {
				return failure
			}
			return nil
		}
		out, err := intent.GC(bg, g.env, intent.GCOpts{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(gcIDs(out.WouldDelete), "x-gone") {
			t.Errorf("%v: x-gone listed for deletion: %+v", failure, out.WouldDelete)
		}
		k := slices.IndexFunc(out.Failed, func(x intent.GCRef) bool { return x.ID == "x-gone" })
		if k < 0 || !strings.HasPrefix(out.Failed[k].Detail, "unread: ") || !out.Negative() {
			t.Errorf("%v: failed = %+v, want x-gone failed as unread, and a negative run", failure, out.Failed)
		}
	}
}

// The re-read under the lock guards closed beads too: one reopened meanwhile
// keeps its ref, and one that cannot be read again is not deleted.
func TestGCReReadsAClosedBeadUnderTheLock(t *testing.T) {
	a, _, g, bd := gcWorld(t)
	shows := map[string]int{}
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "show" {
			shows[args[1]]++
			if shows[args[1]] == 2 { // the re-read under the lock
				switch args[1] {
				case "x-closed":
					bd.beads["x-closed"].Status = "open" // reopened, not claimed
				case "x-landed":
					return errors.New("bd show x-landed --json: exit status 1: database is locked")
				}
			}
		}
		return prev(args)
	}
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-closed", "x-landed"}})
	if err == nil || !strings.Contains(err.Error(), "x-landed") {
		t.Errorf("err = %v, want the x-landed re-read failure", err)
	}
	if gcIDs(out.Kept) != "x-closed" || out.Kept[0].Detail != "changed since gc read it: now open" {
		t.Errorf("kept = %+v", out.Kept)
	}
	if gcIDs(out.Failed) != "x-landed" || !strings.HasPrefix(out.Failed[0].Detail, "re-read: ") || len(out.Deleted) != 0 {
		t.Errorf("failed = %+v, deleted = %+v", out.Failed, out.Deleted)
	}
	for _, id := range []string{"x-closed", "x-landed"} {
		if liveSHA(t, a, id) == "" {
			t.Errorf("gc deleted %s's ref", id)
		}
	}
}

// Naming a kept ref in a real run is a run that left what it meant to delete.
func TestGCOnlyNamingAKeptRefIsNegative(t *testing.T) {
	_, _, g, _ := gcWorld(t)
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-reopened"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Kept) != "x-reopened" || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if out, _ := intent.GC(bg, g.env, intent.GCOpts{DryRun: true, Only: []string{"x-reopened"}}); out.Negative() {
		t.Errorf("a preview is never negative for a kept ref: %+v", out)
	}
}

// An id with no ref at all, as on a rerun after a cut-short run, is done.
func TestGCOnlyAnAbsentRefIsNotNegative(t *testing.T) {
	_, _, g, _ := gcWorld(t)
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-none"}})
	if err != nil || out.Negative() || len(out.Skipped) != 0 || !slices.Contains(out.Warnings, "x-none: no claim ref, so nothing to delete") {
		t.Fatalf("%+v, %v", out, err)
	}
}

// A held bead a person parked (blocked or deferred) is still held, though
// stale reads in-progress beads only and lists its old ref as an orphan.
func TestGCKeepsTheRefOfAParkedClaim(t *testing.T) {
	_, c, g, bd := gcWorld(t)
	n := bd.beads["x-orphan"]
	n.Status, n.Assignee, n.Labels = "blocked", "tester", []string{"run:C"}
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Kept) != "x-orphan" || !strings.Contains(out.Kept[0].Detail, "parked claim") || len(out.Deleted) != 0 {
		t.Fatalf("%+v", out)
	}
	if liveSHA(t, c, "x-orphan") == "" {
		t.Error("gc deleted a parked claim's ref")
	}
}

// Clocks differ between machines: a fresh ref a little older than the close
// may still be a claim of the reopened bead, so it waits for stale_minutes.
func TestGCAllowsForClockSkewOnAClosedBead(t *testing.T) {
	_, _, g, bd := gcWorld(t)
	bd.beads["x-reopened"].Updated = time.Now().UTC().Add(5 * time.Minute) // the closer's clock runs ahead
	out, err := intent.GC(bg, g.env, intent.GCOpts{DryRun: true, Only: []string{"x-reopened"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Kept) != "x-reopened" || len(out.WouldDelete) != 0 {
		t.Fatalf("%+v", out)
	}
}

// A missing bead frees only an orphan's ref: a fresh claim whose bead this
// ledger has not seen yet keeps it.
func TestGCKeepsAYoungRefWhoseBeadIsMissing(t *testing.T) {
	a, _, g, bd := gcWorld(t)
	delete(bd.beads, "x-young")
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "show" && args[1] == "x-young" {
			return errors.New("bd show x-young --json: exit status 1: no issues found matching the provided IDs")
		}
		return prev(args)
	}
	out, err := intent.GC(bg, g.env, intent.GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	k := slices.IndexFunc(out.Kept, func(x intent.GCRef) bool { return x.ID == "x-young" })
	if k < 0 || !strings.Contains(out.Kept[k].Detail, "not an orphan stale lists") {
		t.Fatalf("kept = %+v", out.Kept)
	}
	if liveSHA(t, a, "x-young") == "" {
		t.Error("gc deleted a young ref whose bead is missing")
	}
}

// Stale and gc read the refs apart: a claim that replaced an orphan's ref in
// between is fresh, and gc must not take it for the orphan stale judged.
func TestGCLeavesAClaimThatReplacedAnOrphanAfterStaleRead(t *testing.T) {
	_, c, g, _ := gcWorld(t)
	var fresh string
	g.git.afterSeen = func() { // after stale's read of the refs
		takeOver(t, c, "x-orphan") // dated now
		fresh = liveSHA(t, c, "x-orphan")
		if err := g.env.Lock.Fetch(bg); err != nil {
			t.Fatal(err)
		}
	}
	out, err := intent.GC(bg, g.env, intent.GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gcIDs(out.Deleted), "x-orphan") {
		t.Fatalf("deleted the fresh claim: %+v", out)
	}
	if got := liveSHA(t, c, "x-orphan"); got != fresh {
		t.Errorf("ref = %q, want the fresh claim %q", got, fresh)
	}
}

// A preview already lists what this clone holds under kept, so its
// would_delete is what a real run deletes.
func TestGCPreviewKeepsARefThisCloneHolds(t *testing.T) {
	a, _, _, _ := gcWorld(t)
	out, err := intent.GC(bg, a.env, intent.GCOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gcIDs(out.WouldDelete), "x-closed") {
		t.Errorf("would_delete lists a ref this clone holds: %+v", out.WouldDelete)
	}
	k := slices.IndexFunc(out.Kept, func(x intent.GCRef) bool { return x.ID == "x-closed" })
	if k < 0 || !strings.Contains(out.Kept[k].Detail, "held by run A") {
		t.Errorf("kept = %+v", out.Kept)
	}
}

// A bead another bops process here is working on waits for the next gc: the
// run is negative, not an error, and the ref stays.
func TestGCLeavesABusyBeadForTheNextRun(t *testing.T) {
	a, _, g, _ := gcWorld(t)
	unlock, err := g.store.LockBead("x-orphan")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatalf("a busy bead is not an error: %v", err)
	}
	if gcIDs(out.Failed) != "x-orphan" || !out.Negative() {
		t.Fatalf("%+v", out)
	}
	if liveSHA(t, a, "x-orphan") == "" {
		t.Error("gc deleted a busy bead's ref")
	}
}

// Out of time while judging, the rest is not reached, never quietly kept.
func TestGCOutOfTimeListsTheRestAsFailed(t *testing.T) {
	_, _, g, bd := gcWorld(t)
	ctx, cancel := context.WithCancel(bg)
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "show" && args[1] == "x-closed" {
			cancel() // the budget ends during the first bead's read
		}
		return prev(args)
	}
	out, err := intent.GC(ctx, g.env, intent.GCOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Failed) == 0 || !out.Negative() || !strings.Contains(out.Failed[0].Detail, "not reached") {
		t.Fatalf("%+v", out)
	}
}

// Claims write the ledger beside the lock remote; a Dolt remote on another
// repository is another ledger, where live claims look missing or closed.
func TestGCRefusesALedgerOnAnotherRepository(t *testing.T) {
	a, _, g, bd := gcWorld(t)
	bd.remotes = `[{"name":"origin","url":"git+https://example.invalid/other/repo.git"}]`
	bd.calls = nil
	if _, err := intent.GC(bg, g.env, intent.GCOpts{}); err == nil {
		t.Fatal("gc ran against a ledger on another repository")
	}
	if bd.count("dolt pull") != 0 || liveSHA(t, a, "x-orphan") == "" || liveSHA(t, a, "x-closed") == "" {
		t.Errorf("gc pulled or deleted: %v", bd.calls)
	}
}

// A read cut off by the end of the command's time is not a verdict either.
func TestGCAReadCutOffByTheBudgetIsFailed(t *testing.T) {
	_, _, g, bd := gcWorld(t)
	ctx, cancel := context.WithCancel(bg)
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "show" && args[1] == "x-young" { // the last ref
			cancel()
			return context.Canceled
		}
		return prev(args)
	}
	out, err := intent.GC(ctx, g.env, intent.GCOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	k := slices.IndexFunc(out.Failed, func(x intent.GCRef) bool { return x.ID == "x-young" })
	if k < 0 || !strings.Contains(out.Failed[k].Detail, "not reached") || !out.Negative() {
		t.Fatalf("%+v", out)
	}
}
