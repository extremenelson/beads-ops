package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// Every bd call waits for the one embedded store, so each command writes its
// display's assignee, status and labels in one bd update and its event in one
// comment. These tests count the calls with the recording fake bd.

// writeKinds names each ledger write: "update", "label", "comments add".
func writeKinds(calls [][]string) []string {
	var out []string
	for _, c := range writeCalls(calls) {
		switch {
		case c[0] == "comments" && c[1] == "add":
			out = append(out, "comments add")
		case c[0] == "heartbeat":
		default:
			out = append(out, c[0])
		}
	}
	return out
}

func lastUpdate(t *testing.T, calls [][]string) []string {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i][0] == "update" {
			return calls[i]
		}
	}
	t.Fatalf("no update in %v", calls)
	return nil
}

func TestClaimWritesItsDisplayInOneUpdateAndOneComment(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Labels = []string{"bops:hold"}
	a := client(t, remote, "A", bd)

	out := mustClaim(t, a, "x-1")
	if !out.ReadbackOK || out.LedgerPending {
		t.Fatalf("%+v", out)
	}
	if got := writeKinds(bd.calls); !slices.Equal(got, []string{"update", "comments add"}) {
		t.Errorf("writes = %v, want one update and one comment: %v", got, bd.calls)
	}
	up := lastUpdate(t, bd.calls)
	for _, want := range []string{"--claim", "--add-label run:A", "--remove-label bops:hold"} {
		if !strings.Contains(strings.Join(up, " "), want) {
			t.Errorf("update %v lacks %s", up, want)
		}
	}
	// The update's own output is the readback, and the post-pull show
	// carries the comments the duplicate check needs: no further show or
	// comments read. Remote list, show, pull, show, update, comment, commit,
	// push: 8, against 11 before (and 10 before every push committed first).
	want := []string{"dolt", "show", "dolt", "show", "update", "comments", "dolt", "dolt"}
	if got := callNames(bd.calls); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", bd.calls, want)
	}
	n := bd.beads["x-1"]
	if n.Assignee != "tester" || n.Status != "in_progress" || !slices.Equal(n.Labels, []string{"run:A"}) {
		t.Errorf("display = %+v", n)
	}
}

// A takeover removes every other run's label and the hold labels in the
// same update that moves the assignee.
func TestTakeoverWritesItsDisplayInOneUpdate(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	n := bdB.beads["x-1"]
	n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A", "bops:landing"}
	later(b, twoDays)
	bdB.calls = nil
	mustForce(t, b, "x-1")
	if got := writeKinds(bdB.calls); !slices.Equal(got, []string{"update", "comments add"}) {
		t.Errorf("writes = %v: %v", got, bdB.calls)
	}
	if !slices.Equal(n.Labels, []string{"run:B"}) || n.Assignee != "tester" || n.Status != "in_progress" {
		t.Errorf("display = %+v", n)
	}
}

func TestReleaseWritesItsDisplayInOneUpdateAndOneComment(t *testing.T) {
	for _, tc := range []struct {
		reason, status, label string
	}{
		{"scope", "open", "bops:hold"},
		{"abandoned", "open", "bops:hold"},
		{"human", "open", intent.LabelNeedsHuman},
		{"blocked", "blocked", ""},
		{"done", "in_progress", "bops:landing"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			remote, bd := bareRemote(t), newBD()
			n := bd.add("x-1")
			a := client(t, remote, "A", bd)
			mustClaim(t, a, "x-1")
			if tc.reason == "done" {
				markLanded(n)
			}
			n.Labels = append(n.Labels, "run:STALE") // a second run: label, which goes too
			bd.calls = nil
			out, err := intent.Release(bg, a.env, "x-1", tc.reason)
			if err != nil || out.Outcome != intent.OutcomeReleased || out.LedgerPending {
				t.Fatalf("%+v, %v", out, err)
			}
			if got := writeKinds(bd.calls); !slices.Equal(got, []string{"update", "comments add"}) {
				t.Errorf("writes = %v, want one update and one comment: %v", got, bd.calls)
			}
			want := []string{}
			if tc.label != "" {
				want = []string{tc.label}
			}
			if n.Status != tc.status || n.Assignee != "" || !slices.Equal(n.Labels, want) {
				t.Errorf("display = %+v, want status %s labels %v", n, tc.status, want)
			}
		})
	}
}

func TestSyncRepairWritesTheDisplayInOneUpdate(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	n.Assignee, n.Status, n.Labels = "other", "in_progress", []string{"run:OLD", "bops:hold", "bops:landing"}
	bd.calls = nil
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if got := writeKinds(bd.calls); !slices.Equal(got, []string{"update"}) {
		t.Errorf("writes = %v: %v", got, bd.calls)
	}
	if n.Assignee != "tester" || n.Status != "in_progress" || !slices.Equal(n.Labels, []string{"run:A"}) {
		t.Errorf("display = %+v", n)
	}
}

// A claim whose display stayed pending owes its comment too; the repair's
// show carries the comments, so a second sync adds no second one.
func TestSyncRepairOfAPendingClaimAddsOneComment(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	bd.fail = func(args []string) error {
		if args[0] == "update" {
			return errors.New("database is locked")
		}
		return nil
	}
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || !out.LedgerPending {
		t.Fatalf("%+v, %v", out, err)
	}
	bd.fail = nil
	for range 2 {
		bd.calls = nil
		if _, err := intent.Sync(bg, a.env, false); err != nil {
			t.Fatal(err)
		}
		if bd.count("comments x-1 --json") != 0 {
			t.Errorf("the show with comments should spare the comments read: %v", bd.calls)
		}
	}
	claimed := 0
	for _, c := range n.Comments {
		if strings.HasPrefix(c, "bops:claimed run=A ") {
			claimed++
		}
	}
	if claimed != 1 || !slices.Equal(n.Labels, []string{"run:A"}) || n.Assignee != "tester" {
		t.Errorf("display = %+v", n)
	}
}

func TestSyncClearsALostDisplayInOneUpdate(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	b := client(t, remote, "B", bd)
	mustClaim(t, a, "x-1")
	git(t, b.dir, "push", "-q", "--no-verify", "origin", ":refs/bops/claims/x-1")
	bd.calls = nil
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if got := writeKinds(bd.calls); !slices.Equal(got, []string{"update"}) {
		t.Errorf("writes = %v: %v", got, bd.calls)
	}
	if n.Status != "open" || n.Assignee != "" || len(n.Labels) != 0 {
		t.Errorf("display = %+v", n)
	}
}

// With the lock off, bd's compare-and-set on update --claim decides, and the
// labels ride on it, so a lost claim writes none of them.
func TestLockOffLabelsRideOnTheClaim(t *testing.T) {
	bd := newBD()
	bd.remotes = `[{"name":"origin","url":"git+file:///nowhere"}]` // a ledger with somewhere to publish
	bd.add("x-1").Labels = []string{"bops:hold"}
	r := lockOffRig(t, "A", bd)
	out, err := intent.Claim(bg, r.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeClaimed || !out.ReadbackOK {
		t.Fatalf("%+v, %v", out, err)
	}
	if got := writeKinds(bd.calls); !slices.Equal(got, []string{"update", "comments add"}) {
		t.Errorf("writes = %v: %v", got, bd.calls)
	}
	up := strings.Join(lastUpdate(t, bd.calls), " ")
	if !strings.HasPrefix(up, "update x-1 --claim ") || !strings.Contains(up, "--add-label run:A") || !strings.Contains(up, "--remove-label bops:hold") {
		t.Errorf("update = %s", up)
	}
	// remote list, pull, show, update --claim, comment, commit, push.
	if len(bd.calls) != 7 {
		t.Errorf("calls = %v, want 7", bd.calls)
	}

	lost := newBD()
	lost.add("x-2")
	lr := lockOffRig(t, "B", lost)
	lost.fail = func(args []string) error {
		if args[0] == "update" && args[2] == "--claim" {
			lost.beads["x-2"].Assignee, lost.beads["x-2"].Status = "other", "in_progress" // another worktree won
			return errors.New("issue already claimed by other")
		}
		return nil
	}
	out, err = intent.Claim(bg, lr.env, "x-2", false)
	if err != nil || out.Outcome != intent.OutcomeLost {
		t.Fatalf("%+v, %v", out, err)
	}
	if got := lost.beads["x-2"]; len(got.Labels) != 0 || len(got.Comments) != 0 {
		t.Errorf("a lost claim wrote its display: %+v", got)
	}
}

// failConfirm is a journal whose Confirm fails, so the claim it records is
// undone on the ledger.
type failConfirm struct{ *runstate.Store }

func (failConfirm) Confirm(string, string) error { return errors.New("disk full") }

// A lock-off claim whose hold cannot be journaled is undone, labels included,
// so the bead shows what it showed before.
func TestLockOffUndoRemovesTheLabelsThatRodeOnTheClaim(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Labels = []string{"bops:hold"}
	r := lockOffRig(t, "A", bd)
	r.env.Lock.Journal = failConfirm{r.store}
	if out, err := intent.Claim(bg, r.env, "x-1", false); err == nil {
		t.Fatalf("want the journal error: %+v", out)
	}
	if n.Status != "open" || n.Assignee != "" || !slices.Equal(n.Labels, []string{"bops:hold"}) {
		t.Errorf("display = %+v, want it as before the claim", n)
	}
}

// omitComments is a bd whose show leaves the comments out even when asked.
type omitComments struct{ *fakeBD }

func (w omitComments) Run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := w.fakeBD.Run(ctx, args...)
	if err == nil && args[0] == "show" && slices.Contains(args, "--include-comments") {
		var v []map[string]any
		if json.Unmarshal(out, &v) == nil && len(v) == 1 {
			delete(v[0], "comments")
			v[0]["comments_omitted"] = true
			out, _ = json.Marshal(v)
		}
	}
	return out, err
}

// When bd leaves comments out, the duplicate check reads them itself, so a
// retry still adds no second bops:claimed comment.
func TestClaimReadsTheCommentsWhenShowOmitsThem(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	n.Comments = []string{"bops:claimed run=A actor=tester machine=box"} // an earlier try's
	a := client(t, remote, "A", bd)
	a.env.BD = omitComments{bd}
	mustClaim(t, a, "x-1")
	if bd.count("comments x-1 --json") != 1 || len(n.Comments) != 1 {
		t.Errorf("comments = %v, calls = %v", n.Comments, bd.calls)
	}
}

// plainUpdate is a bd whose update prints nothing that decodes.
type plainUpdate struct{ *fakeBD }

func (p plainUpdate) Run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := p.fakeBD.Run(ctx, args...)
	if args[0] == "update" {
		return nil, err
	}
	return out, err
}

// With no update output to read back, the claim shows the bead again.
func TestReadbackFallsBackToAShow(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	a.env.BD = plainUpdate{bd}
	if out := mustClaim(t, a, "x-1"); !out.ReadbackOK || out.LedgerPending {
		t.Fatalf("%+v", out)
	}
	if i := slices.IndexFunc(bd.calls, func(c []string) bool { return c[0] == "update" }); i < 0 || !slices.ContainsFunc(bd.calls[i:], func(c []string) bool { return c[0] == "show" }) {
		t.Errorf("no show after the update: %v", bd.calls)
	}

	off := newBD()
	off.add("x-2")
	r := lockOffRig(t, "B", off)
	r.env.BD = plainUpdate{off}
	out, err := intent.Claim(bg, r.env, "x-2", false)
	if err != nil || out.Outcome != intent.OutcomeClaimed || !out.ReadbackOK {
		t.Fatalf("lock off: %+v, %v", out, err)
	}
	if got := off.beads["x-2"]; !slices.Equal(got.Labels, []string{"run:B"}) || got.Assignee != "tester" {
		t.Errorf("lock off display = %+v", got)
	}
}

// A failed undo names the whole command that undoes the claim, labels included.
func TestLockOffFailedUndoNamesTheWholeUndo(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Labels = []string{"bops:hold"}
	r := lockOffRig(t, "A", bd)
	r.env.Lock.Journal = failConfirm{r.store}
	bd.fail = func(args []string) error {
		if args[0] == "update" && slices.Contains(args, "open") {
			return errors.New("database is locked")
		}
		return nil
	}
	out, err := intent.Claim(bg, r.env, "x-1", false)
	if err == nil {
		t.Fatalf("want the journal error: %+v", out)
	}
	want := "bd update x-1 --assignee '' --status open --add-label bops:hold --remove-label run:A"
	if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
		t.Errorf("warnings = %v, want %q", out.Warnings, want)
	}
}

// bd reads --add-label and --remove-label values as CSV, so a stale run:
// label with a quote in it is quoted: it neither stops the release nor
// removes the wrong label. (bd trims and splits at commas when it stores a
// label, so the comma here only shows a quoted value stays whole.)
func TestAStrangeRunLabelDoesNotStopTheDisplayWrite(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	n.Labels = append(n.Labels, `run:x"y`, "run:a,b", "run:a")
	out, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeReleased || out.LedgerPending {
		t.Fatalf("%+v, %v", out, err)
	}
	if !slices.Equal(n.Labels, []string{"bops:hold"}) {
		t.Errorf("labels = %q", n.Labels)
	}
}
