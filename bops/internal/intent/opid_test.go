package intent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// loseAnswer lets bd's matching writes land, then fails them, as a timeout
// or a dropped connection does after the write reached the ledger.
type loseAnswer struct {
	*fakeBD
	match func(args []string) bool
	n     int // how many answers to lose
}

func (l *loseAnswer) Run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := l.fakeBD.Run(ctx, args...)
	if err == nil && l.n > 0 && l.match(args) {
		l.n--
		return nil, errors.New("signal: killed")
	}
	return out, err
}

func isCommentAdd(args []string) bool { return args[0] == "comments" && args[1] == "add" }

func countPrefix(comments []string, prefix string) int {
	n := 0
	for _, c := range comments {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// A note whose answer was lost after it landed is retried, and the retry
// writes nothing and answers noted, even once the bead is given back.
func TestNoteRetriedAfterItLandedIsANoOp(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	a.env.BD = &loseAnswer{fakeBD: bd, match: isCommentAdd, n: 1}
	text := "did a thing\n\nNEXT: the next thing"
	if _, err := intent.Note(bg, a.env, "x-1", text); err == nil {
		t.Fatal("the lost answer should fail the first try")
	}
	out, err := intent.Note(bg, a.env, "x-1", text)
	if err != nil || out.Outcome != intent.OutcomeNoted || !out.Replayed || out.OpID == "" {
		t.Fatalf("%+v, %v", out, err)
	}
	if n := countPrefix(bd.beads["x-1"].Comments, "bops:note "); n != 1 {
		t.Errorf("notes = %d, want 1: %v", n, bd.beads["x-1"].Comments)
	}
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	// The note landed, but the run no longer holds the bead: refused, replayed.
	if out, err := intent.Note(bg, a.env, "x-1", text); err != nil || !out.Replayed || out.Outcome != intent.OutcomeRefused || !out.Negative() {
		t.Fatalf("after release: %+v, %v", out, err)
	}
	// Different text is a different operation: refused, since the hold is gone.
	if out, err := intent.Note(bg, a.env, "x-1", text+" more"); err != nil || out.Outcome != intent.OutcomeRefused || out.Replayed {
		t.Fatalf("new text: %+v, %v", out, err)
	}
}

func TestNoteOpIDNamesTheOperation(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	text := "same\n\nNEXT: same"
	first, err := intent.NoteOp(bg, a.env, "x-1", text, "loop-7.note-1")
	if err != nil || first.Replayed || first.OpID != "loop-7.note-1" {
		t.Fatalf("%+v, %v", first, err)
	}
	if out, err := intent.NoteOp(bg, a.env, "x-1", text, "loop-7.note-1"); err != nil || !out.Replayed {
		t.Fatalf("repeat: %+v, %v", out, err)
	}
	if out, err := intent.NoteOp(bg, a.env, "x-1", text, "loop-7.note-2"); err != nil || out.Replayed {
		t.Fatalf("new op: %+v, %v", out, err)
	}
	c := bd.beads["x-1"].Comments
	if n := countPrefix(c, "bops:note "); n != 2 {
		t.Errorf("notes = %d, want 2: %v", n, c)
	}
	if !strings.HasPrefix(c[len(c)-1], "bops:note run=A op=loop-7.note-2\n") {
		t.Errorf("header = %q", c[len(c)-1])
	}
}

func TestBadOpIDIsRefusedBeforeAnyCall(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	before := len(bd.calls)
	for _, op := range []string{"has space", "-flag", "semi;colon", strings.Repeat("a", 65)} {
		if _, err := intent.NoteOp(bg, a.env, "x-1", "t\n\nNEXT: x", op); !errors.Is(err, intent.ErrPrecondition) {
			t.Errorf("note %q: %v", op, err)
		}
		if _, err := intent.ReleaseOp(bg, a.env, "x-1", "scope", "", op); !errors.Is(err, intent.ErrPrecondition) {
			t.Errorf("release %q: %v", op, err)
		}
		if _, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: "https://github.com/o/r/pull/1", SHA: "abc1234", OpID: op}); !errors.Is(err, intent.ErrPrecondition) {
			t.Errorf("land %q: %v", op, err)
		}
	}
	if len(bd.calls) != before {
		t.Errorf("calls: %v", bd.calls[before:])
	}
}

// A release whose push was rejected, or whose answer was lost, is retried:
// the retry writes nothing and answers released.
func TestReleaseRetriedAfterItLandedIsANoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(*rig)
	}{
		{"push rejected", func(a *rig) { rejectPushes(a.bd, 100) }},
		{"answer lost", func(a *rig) {
			a.env.BD = &loseAnswer{fakeBD: a.bd, n: 1, match: func(args []string) bool {
				return isCommentAdd(args) && args[3] == "-f"
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote, bd := bareRemote(t), newBD()
			bd.add("x-1")
			a := client(t, remote, "A", bd)
			mustClaim(t, a, "x-1")
			tc.lose(a)
			text := "BLOCKER: waiting on a key"
			if out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", text); err != nil || out.Outcome != intent.OutcomeReleased {
				t.Fatalf("first: %+v, %v", out, err)
			}
			bd.fail = nil
			a.env.BD = bd
			before, pushes := len(bd.calls), bd.count("dolt push")
			out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", text)
			if err != nil || out.Outcome != intent.OutcomeReleased || !out.Replayed || out.Reason != "blocked" || out.Negative() {
				t.Fatalf("retry: %+v, %v", out, err)
			}
			if w := writeCalls(bd.calls[before:]); len(w) != 0 {
				t.Errorf("retry wrote: %v", w)
			}
			// The first run's push may never have happened: the retry publishes.
			if bd.count("dolt push") == pushes || out.LedgerPending {
				t.Errorf("retry did not publish: %+v", out)
			}
			if n := countPrefix(bd.beads["x-1"].Comments, "bops:released "); n != 1 {
				t.Errorf("released comments = %d: %v", n, bd.beads["x-1"].Comments)
			}
			// Another reason, even under the same op id, is not this release.
			if out, err := intent.ReleaseOp(bg, a.env, "x-1", "scope", text, out.OpID); err != nil || out.Outcome != intent.OutcomeRefused || out.Replayed {
				t.Fatalf("same op, other reason: %+v, %v", out, err)
			}
			// Another reason is another operation, refused as before.
			if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeRefused || out.Replayed {
				t.Fatalf("other reason: %+v, %v", out, err)
			}
		})
	}
}

// While the hold remains, a release runs in full, even when this run's
// earlier release carries its op id: a same-run re-claim released again with
// the same reason keeps its own bops:released record.
func TestReleaseStillHeldRunsInFull(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased || out.Replayed {
		t.Fatalf("first: %+v, %v", out, err)
	}
	n.Labels = nil // the cooling-off hold would refuse the re-claim
	mustClaim(t, a, "x-1")
	out, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeReleased || out.Replayed {
		t.Fatalf("second: %+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("ref should be gone")
	}
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("hold should be gone")
	}
	if c := countPrefix(n.Comments, "bops:released "); c != 2 {
		t.Errorf("released comments = %d: %v", c, n.Comments)
	}
}

// Only this run's comment is a replay: another run that picked the same
// --op-id, or a comment anyone wrote, never stands in for a write.
func TestAnotherRunsOpIDIsNotAReplay(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	bd.add("x-2").Comments = []string{"bops:released run=OTHER reason=done op=step-2"}
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	n.Comments = append(n.Comments,
		"bops:note run=OTHER op=step-1\nNEXT: theirs",
		"bops:landed run=OTHER ref=https://github.com/o/r/pull/9 sha=abc1234 op=step-3")
	out, err := intent.NoteOp(bg, a.env, "x-1", "mine\n\nNEXT: mine", "step-1")
	if err != nil || out.Replayed || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("note: %+v, %v", out, err)
	}
	if c := countPrefix(n.Comments, "bops:note run=A op=step-1\n"); c != 1 {
		t.Errorf("own note not written: %v", n.Comments)
	}
	if out, err := intent.ReleaseOp(bg, a.env, "x-2", "scope", "", "step-2"); err != nil || out.Replayed || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("release of a bead not held: %+v, %v", out, err)
	}
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	if out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: "https://github.com/o/r/pull/9", SHA: "abc1234", OpID: "step-3"}); err != nil || out.Replayed || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("land: %+v, %v", out, err)
	}
}

// A land replay answers from the landed record: this run's landing comment
// with no landed metadata behind it is not proof the landing happened.
func TestLandReplayNeedsTheLandedRecord(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	n.Comments = append(n.Comments, "bops:landed run=A ref=https://github.com/o/r/pull/9 sha=abc1234 op=op-9")
	out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: "https://github.com/o/r/pull/9", SHA: "abc1234", OpID: "op-9"})
	if err != nil || out.Replayed || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
}

// A replayed note keeps the claim alive, as a note does, and says so when the
// bead is no longer held.
func TestReplayedNoteStillRefreshesTheClaim(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	text := "same\n\nNEXT: same"
	if _, err := intent.Note(bg, a.env, "x-1", text); err != nil {
		t.Fatal(err)
	}
	before, beats := liveSHA(t, a, "x-1"), bd.count("heartbeat")
	out, err := intent.Note(bg, a.env, "x-1", text)
	if err != nil || !out.Replayed {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") == before {
		t.Error("the claim ref was not refreshed")
	}
	if bd.count("heartbeat") != beats+1 {
		t.Error("no heartbeat")
	}
	if c := countPrefix(bd.beads["x-1"].Comments, "bops:note "); c != 1 {
		t.Errorf("notes = %d", c)
	}
	takeOver(t, client(t, remote, "B", bd), "x-1")
	out, err = intent.Note(bg, a.env, "x-1", text)
	if err != nil || !out.Replayed || out.Holder == nil || out.Outcome != intent.OutcomeLost || !out.Negative() {
		t.Fatalf("lost: %+v, %v", out, err)
	}
}

// A failed comment read is a warning, and the write goes ahead once.
func TestOpCheckReadFailureIsAWarning(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.fail = func(args []string) error {
		if len(args) == 3 && args[0] == "comments" && args[2] == "--json" {
			return errors.New("database is locked")
		}
		return nil
	}
	out, err := intent.Note(bg, a.env, "x-1", "t\n\nNEXT: x")
	if err != nil || out.Replayed || out.Outcome != intent.OutcomeNoted || !strings.Contains(strings.Join(out.Warnings, ";"), "earlier notes not checked") {
		t.Fatalf("%+v, %v", out, err)
	}
	if c := countPrefix(bd.beads["x-1"].Comments, "bops:note "); c != 1 {
		t.Errorf("notes = %d", c)
	}
	// Not held: a release or land that cannot check for its op is refused.
	bd.fail = nil
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	bd.fail = func(args []string) error {
		if len(args) == 3 && args[0] == "comments" && args[2] == "--json" {
			return errors.New("database is locked")
		}
		return nil
	}
	rel, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || rel.Replayed || rel.Outcome != intent.OutcomeRefused || !strings.Contains(strings.Join(rel.Warnings, ";"), "earlier releases not checked") {
		t.Fatalf("release retry: %+v, %v", rel, err)
	}
	land, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: "https://github.com/o/r/pull/9", SHA: "abc1234"})
	if err != nil || land.Replayed || land.Outcome != intent.OutcomeRefused || !strings.Contains(strings.Join(land.Warnings, ";"), "earlier landings not checked") {
		t.Fatalf("land: %+v, %v", land, err)
	}
}

// A landing whose event comment failed gets it on the repeat, so a retry after
// the release can still be answered.
func TestLandRepeatPostsAMissingEvent(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	o := intent.LandOpts{Ref: "https://github.com/o/r/pull/9", SHA: "abc1234"}
	failed := false
	bd.fail = func(args []string) error {
		if !failed && args[0] == "comments" && args[1] == "add" && strings.HasPrefix(args[3], "bops:landed ") {
			failed = true
			return errors.New("timeout")
		}
		return nil
	}
	if _, err := intent.Land(bg, a.env, "x-1", o); err == nil {
		t.Fatal("the event failure should be reported")
	}
	out, err := intent.Land(bg, a.env, "x-1", o)
	if err != nil || !out.Replayed {
		t.Fatalf("%+v, %v", out, err)
	}
	if c := countPrefix(bd.beads["x-1"].Comments, "bops:landed run=A "); c != 1 {
		t.Errorf("landed comments = %d", c)
	}
	if out, err := intent.Release(bg, a.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	if out, err := intent.Land(bg, a.env, "x-1", o); err != nil || !out.Replayed {
		t.Fatalf("after release: %+v, %v", out, err)
	}
}

// land is idempotent by ref and sha; its op id also answers a landing retried
// after the release that followed it.
func TestLandRetriedAfterReleaseIsANoOp(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	o := intent.LandOpts{Ref: "https://github.com/o/r/pull/9", SHA: "abc1234"}
	first, err := intent.Land(bg, a.env, "x-1", o)
	if err != nil || first.Outcome != intent.OutcomeLanded || first.Replayed {
		t.Fatalf("%+v, %v", first, err)
	}
	pushes := bd.count("dolt push")
	if out, err := intent.Land(bg, a.env, "x-1", o); err != nil || !out.Replayed || out.OpID != first.OpID {
		t.Fatalf("held repeat: %+v, %v", out, err)
	}
	if bd.count("dolt push") == pushes {
		t.Error("the held repeat did not publish")
	}
	if out, err := intent.Release(bg, a.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
	pushes = bd.count("dolt push")
	out, err := intent.Land(bg, a.env, "x-1", o)
	if err != nil || out.Outcome != intent.OutcomeLanded || !out.Replayed || out.Landed.Ref != o.Ref || out.Landed.SHA != o.SHA || out.Landed.Run != "A" {
		t.Fatalf("after release: %+v, %v", out, err)
	}
	if bd.count("dolt push") == pushes {
		t.Error("the replay did not publish")
	}
	// The same op id for another ref or sha is not this landing.
	other := intent.LandOpts{Ref: o.Ref, SHA: "def5678", OpID: out.OpID}
	if out, err := intent.Land(bg, a.env, "x-1", other); err != nil || out.Replayed || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("other sha: %+v, %v", out, err)
	}
	// A replay whose push is still rejected answers its result with a warning.
	rejectPushes(bd, 100)
	out, err = intent.Land(bg, a.env, "x-1", o)
	if err != nil || !out.Replayed || !strings.Contains(strings.Join(out.Warnings, ";"), "not published") {
		t.Fatalf("push rejected: %+v, %v", out, err)
	}
	if c := countPrefix(bd.beads["x-1"].Comments, "bops:landed "); c != 1 {
		t.Errorf("landed comments = %d", c)
	}
}

// A release whose ref delete failed is finished, and replayed, by its rerun.
func TestReleaseRetriedAfterAPendingDeleteIsReplayed(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	a.env.Lock.Git = &deleteFails{Git: a.git}
	if out, err := intent.Release(bg, a.env, "x-1", "blocked"); err != nil || !out.RefPending {
		t.Fatalf("first: %+v, %v", out, err)
	}
	a.env.Lock.Git = a.git
	before := len(bd.calls)
	out, err := intent.Release(bg, a.env, "x-1", "blocked")
	if err != nil || out.Outcome != intent.OutcomeReleased || !out.Replayed || out.SavedTo != "" {
		t.Fatalf("retry: %+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("ref should be gone")
	}
	if w := writeCalls(bd.calls[before:]); len(w) != 0 {
		t.Errorf("retry wrote: %v", w)
	}
	if c := countPrefix(bd.beads["x-1"].Comments, "bops:released "); c != 1 {
		t.Errorf("released comments = %d", c)
	}
}

// A replayed release whose push is still rejected says so, and saves no text.
func TestReleaseReplayWithARejectedPushIsAWarning(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	rejectPushes(bd, 100)
	text := "BLOCKER: x"
	if _, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", text); err != nil {
		t.Fatal(err)
	}
	out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", text)
	if err != nil || !out.Replayed || out.LedgerPending || out.SavedTo != "" || !strings.Contains(strings.Join(out.Warnings, ";"), "not published") {
		t.Fatalf("%+v, %v", out, err)
	}
}
