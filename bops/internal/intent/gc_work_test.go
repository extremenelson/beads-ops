package intent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// Before deleting an orphan's ref, or a closed bead's ref newer than the
// bead's last ledger change, gc reads reap's work evidence: such a ref may be
// a live claim whose ledger display never reached this ledger.

func gcKeptDetail(out *intent.GCOut, id string) string {
	if k := slices.IndexFunc(out.Kept, func(x intent.GCRef) bool { return x.ID == id }); k >= 0 {
		return out.Kept[k].Detail
	}
	return ""
}

func TestGCKeepsAnOrphanWhoseBeadHasABranch(t *testing.T) {
	a, c, g, _ := gcWorld(t)
	pushBranch(t, c, "work/x-orphan")
	pushBranch(t, c, "feat/x-gone-wip") // a bead this ledger has not seen
	out, err := intent.GC(bg, g.env, intent.GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"x-orphan", "x-gone"} {
		if d := gcKeptDetail(out, id); !strings.Contains(d, "shows work") || !strings.Contains(d, "branch ") || !strings.Contains(d, id) {
			t.Errorf("%s kept detail = %q; out = %+v", id, d, out)
		}
		if liveSHA(t, a, id) == "" {
			t.Errorf("gc deleted %s's ref, whose bead has a branch", id)
		}
	}
	if got := gcIDs(out.Deleted); got != "x-closed,x-landed" {
		t.Errorf("deleted = %q", got)
	}
	if !slices.Contains(out.Warnings, intent.ReapNoForge) {
		t.Errorf("no forge, and no warning saying so: %v", out.Warnings)
	}
}

func TestGCKeepsAnOrphanLabelledOrWithAnMROnTheBead(t *testing.T) {
	_, _, g, bd := gcWorld(t)
	bd.beads["x-orphan"].Labels = []string{"needs-land"}
	out, err := intent.GC(bg, g.env, intent.GCOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := gcKeptDetail(out, "x-orphan"); !strings.Contains(d, "label needs-land") {
		t.Errorf("kept detail = %q", d)
	}
	bd.beads["x-orphan"].Labels = nil
	bd.beads["x-orphan"].Comments = []string{"MR: https://github.com/o/r/pull/7"}
	out, err = intent.GC(bg, g.env, intent.GCOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := gcKeptDetail(out, "x-orphan"); !strings.Contains(d, "mr https://github.com/o/r/pull/7") {
		t.Errorf("kept detail = %q", d)
	}
}

func TestGCKeepsOrphansWhenTheForgeDoesNotAnswer(t *testing.T) {
	a, _, g, _ := gcWorld(t)
	f := &searchForge{err: errors.New("gh: not logged in")}
	out, err := intent.GC(bg, g.env, intent.GCOpts{Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"x-orphan", "x-gone"} {
		if d := gcKeptDetail(out, id); !strings.Contains(d, "forge unanswered") {
			t.Errorf("%s kept detail = %q", id, d)
		}
		if liveSHA(t, a, id) == "" {
			t.Errorf("gc deleted %s's ref on an unanswered forge", id)
		}
	}
	if len(f.asked) != 1 {
		t.Errorf("a dead forge is asked once: %v", f.asked)
	}
	if !slices.Contains(out.Warnings, intent.GCForgeUnanswered) {
		t.Errorf("warnings = %v", out.Warnings)
	}
	// The closed beads' refs predate their close: no evidence is read.
	if got := gcIDs(out.Deleted); got != "x-closed,x-landed" {
		t.Errorf("deleted = %q", got)
	}
}

func TestGCDeletesAnOrphanWithNoWorkAfterAskingTheForge(t *testing.T) {
	a, _, g, _ := gcWorld(t)
	f := &searchForge{mrs: map[string][]forge.MR{
		"x-orphan": {{URL: "https://github.com/o/r/pull/4", Title: "x-orphan attempt", State: "closed"}},
	}}
	out, err := intent.GC(bg, g.env, intent.GCOpts{Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if got := gcIDs(out.Deleted); got != "x-closed,x-gone,x-landed,x-orphan" || out.Negative() {
		t.Fatalf("deleted = %q: %+v", got, out)
	}
	for _, id := range []string{"x-orphan", "x-gone"} {
		// A real run asks only under the bead lock: once per ref.
		if n := slices.Index(f.asked, id); n < 0 || slices.Index(f.asked[n+1:], id) >= 0 {
			t.Errorf("forge asked about %s %d times: %v", id, n, f.asked)
		}
		if liveSHA(t, a, id) != "" {
			t.Errorf("%s's ref survived gc", id)
		}
	}
	if slices.Contains(out.Warnings, intent.ReapNoForge) {
		t.Errorf("warned of no forge with one: %v", out.Warnings)
	}
}

// A closed bead's ref newer than the bead's last ledger change may be the
// claim of a reopened bead, however old the ref; one older than the change is
// the leftover of a closed bead, whatever branches it has.
func TestGCChecksWorkOnAClosedBeadWhoseRefIsNewerThanItsLedgerChange(t *testing.T) {
	a, c, g, bd := gcWorld(t)
	bd.beads["x-orphan"].Status = "closed" // Updated stays at the fixture's old date, before the 3h-old ref
	pushBranch(t, c, "work/x-orphan")
	pushBranch(t, c, "work/x-closed") // x-closed was closed after its ref was written
	out, err := intent.GC(bg, g.env, intent.GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if d := gcKeptDetail(out, "x-orphan"); !strings.Contains(d, "branch work/x-orphan") {
		t.Errorf("kept detail = %q; out = %+v", d, out)
	}
	if liveSHA(t, a, "x-orphan") == "" {
		t.Error("gc deleted a reopened claim's ref with a branch")
	}
	if !strings.Contains(gcIDs(out.Deleted), "x-closed") {
		t.Errorf("deleted = %q", gcIDs(out.Deleted))
	}

	// With no evidence, the same ref goes, as closed.
	_, _, g2, bd2 := gcWorld(t)
	bd2.beads["x-orphan"].Status = "closed"
	out, err = intent.GC(bg, g2.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Deleted) != 1 || out.Deleted[0].ID != "x-orphan" || out.Deleted[0].Why != intent.GCClosed {
		t.Fatalf("%+v", out)
	}
}

// The evidence is read again under the bead's lock, on a fresh branch listing
// and a fresh forge search: work that appeared since keeps the ref.
func TestGCReChecksWorkUnderTheLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch bool
	}{{"branch", true}, {"forge", false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, g, bd := gcWorld(t)
			f := &searchForge{}
			if !tc.branch {
				// A real run's only search is under the lock.
				f.mrs = map[string][]forge.MR{"x-orphan": {{URL: "https://github.com/o/r/pull/8", Title: "x-orphan", State: "open"}}}
			}
			shows := 0
			prev := bd.fail
			bd.fail = func(args []string) error {
				if args[0] == "show" && args[1] == "x-orphan" {
					if shows++; shows == 2 && tc.branch { // the re-read under the lock
						pushBranch(t, c, "work/x-orphan")
					}
				}
				return prev(args)
			}
			out, err := intent.GC(bg, g.env, intent.GCOpts{Only: []string{"x-orphan"}, Forge: f})
			if err != nil {
				t.Fatal(err)
			}
			if d := gcKeptDetail(out, "x-orphan"); !strings.Contains(d, "shows work") || len(out.Deleted) != 0 || !out.Negative() {
				t.Fatalf("%+v", out)
			}
			if liveSHA(t, c, "x-orphan") == "" {
				t.Error("gc deleted a ref whose work appeared under the lock")
			}
		})
	}
}

// A branch listing that fails is no verdict: the refs that need one are
// listed under failed, and nothing else is held up.
func TestGCListsAnOrphanAsFailedWhenBranchesCannotBeListed(t *testing.T) {
	a, _, g, _ := gcWorld(t)
	g.env.Lock.Git = &headsFail{Git: g.env.Lock.Git, n: 1}
	out, _ := intent.GC(bg, g.env, intent.GCOpts{})
	if got := gcIDs(out.Failed); got != "x-gone,x-orphan" || !out.Negative() {
		t.Fatalf("failed = %q: %+v", got, out)
	}
	if !strings.Contains(out.Failed[0].Detail, "branches could not be listed") {
		t.Errorf("detail = %q", out.Failed[0].Detail)
	}
	for _, id := range []string{"x-orphan", "x-gone"} {
		if liveSHA(t, a, id) == "" {
			t.Errorf("gc deleted %s's ref unchecked", id)
		}
	}
	if got := gcIDs(out.Deleted); got != "x-closed,x-landed" {
		t.Errorf("deleted = %q", got)
	}

	// Under the lock too: a listing that fails there deletes nothing.
	_, _, g2, _ := gcWorld(t)
	g2.env.Lock.Git = &headsFail{Git: g2.env.Lock.Git, n: 2}
	out, err := intent.GC(bg, g2.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err == nil || len(out.Deleted) != 0 || gcIDs(out.Failed) != "x-orphan" || !strings.HasPrefix(out.Failed[0].Detail, "re-check: ") {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
}

// A preview with too little time left for a forge search lists the ref under
// failed, not kept on a search the deadline would cut short.
func TestGCPreviewWithNoTimeForTheForgeListsTheRefAsFailed(t *testing.T) {
	_, _, g, _ := gcWorld(t)
	f := &searchForge{block: true}
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	out, err := intent.GC(ctx, g.env, intent.GCOpts{DryRun: true, Only: []string{"x-orphan"}, Forge: f, ForgeClock: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Failed) != "x-orphan" || !strings.HasPrefix(out.Failed[0].Detail, "not reached") || len(out.Kept)+len(out.WouldDelete)+len(out.Skipped) != 0 {
		t.Fatalf("%+v", out)
	}
	if len(f.asked) != 0 {
		t.Errorf("searched with no time for it: %v", f.asked)
	}
}

// A re-check under the lock that the deadline cuts short deletes nothing and
// is no error: the ref waits for the next run.
func TestGCReCheckCutShortUnderTheLockDeletesNothing(t *testing.T) {
	_, c, g, bd := gcWorld(t)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	shows := 0
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "show" && args[1] == "x-orphan" {
			if shows++; shows == 2 { // the re-read under the lock
				cancel()
			}
		}
		return prev(args)
	}
	out, err := intent.GC(ctx, g.env, intent.GCOpts{Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatal(err)
	}
	if gcIDs(out.Failed) != "x-orphan" || out.Failed[0].Detail != "re-check: not reached: the command's time ran out; run gc again" || len(out.Deleted)+len(out.Skipped) != 0 {
		t.Fatalf("%+v", out)
	}
	if liveSHA(t, c, "x-orphan") == "" {
		t.Error("gc deleted a ref whose re-check was cut short")
	}
}

// Evidence that cannot be read keeps the ref, as in reap, and says so rather
// than claim the bead shows work.
func TestGCKeepsAnOrphanWhoseEvidenceCannotBeRead(t *testing.T) {
	_, _, g, bd := gcWorld(t)
	prev := bd.fail
	bd.fail = func(args []string) error {
		if args[0] == "comments" && args[1] == "x-orphan" {
			return errors.New("dolt: database is locked")
		}
		return prev(args)
	}
	out, err := intent.GC(bg, g.env, intent.GCOpts{DryRun: true, Only: []string{"x-orphan"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := gcKeptDetail(out, "x-orphan"); !strings.HasPrefix(d, "its work evidence could not be read, so kept: ") || strings.Contains(d, "shows work") {
		t.Fatalf("kept detail = %q: %+v", d, out)
	}
}

// An id --only named that gc lists under failed is not listed under skipped
// too.
func TestGCOnlyListsAFailedRefOnce(t *testing.T) {
	_, _, g, _ := gcWorld(t)
	g.env.Lock.Git = &headsFail{Git: g.env.Lock.Git, n: 1}
	out, _ := intent.GC(bg, g.env, intent.GCOpts{DryRun: true, Only: []string{"x-orphan"}})
	if gcIDs(out.Failed) != "x-orphan" || len(out.Skipped) != 0 {
		t.Fatalf("%+v", out)
	}
}
