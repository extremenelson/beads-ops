package intent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// Synthetic fixtures in the shape of bd 1.3.x --json output. Field names
// mirror real output; contents are invented.
const readyJSON = `[
 {"id":"x-1","title":"leaf, workable","description":"d","acceptance_criteria":"it works","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0,"parent":"x-epic",
  "dependencies":[{"issue_id":"x-1","depends_on_id":"x-epic","type":"parent-child"}]},
 {"id":"x-2","title":"p0 but no acceptance","description":"d","status":"open","priority":0,"issue_type":"bug","owner":"o","created_at":"2026-10-02T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0},
 {"id":"x-3","title":"stale run label","description":"d","acceptance_criteria":"ac","status":"open","priority":1,"issue_type":"task","owner":"o","labels":["run:OLDRUN"],"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0},
 {"id":"x-4","title":"needs human","description":"d","acceptance_criteria":"ac","status":"open","priority":0,"issue_type":"task","owner":"o","labels":["needs:human"],"created_at":"2026-10-03T00:00:00Z","updated_at":"2026-10-03T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0},
 {"id":"x-5","title":"needs gpu","description":"d","acceptance_criteria":"ac","status":"open","priority":1,"issue_type":"task","owner":"o","labels":["needs:gpu"],"created_at":"2026-10-03T00:00:00Z","updated_at":"2026-10-03T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0},
 {"id":"x-6","title":"other pool","description":"d","acceptance_criteria":"ac","status":"open","priority":1,"issue_type":"task","owner":"o","labels":["pool:docs"],"created_at":"2026-10-03T00:00:00Z","updated_at":"2026-10-03T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0},
 {"id":"x-7","title":"same prio, older, orphan","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-09-15T00:00:00Z","updated_at":"2026-09-15T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0},
 {"id":"x-epic","title":"the epic","description":"d","acceptance_criteria":"ac","status":"open","priority":0,"issue_type":"epic","owner":"o","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z","dependency_count":0,"dependent_count":2,"comment_count":0}
]`

const inProgressJSON = `[
 {"id":"x-9","title":"sibling in flight","description":"d","status":"in_progress","priority":2,"issue_type":"task","owner":"o","assignee":"someone","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z","started_at":"2026-10-01T00:00:00Z","dependency_count":0,"dependent_count":0,"comment_count":0,"parent":"x-epic"}
]`

func newEnv(f bdx.Runner) *intent.Env {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	return &intent.Env{
		BD:    f,
		Run:   &runstate.Run{ID: "RUN1", Actor: "tester", Machine: "box"},
		Now:   func() time.Time { return now },
		Sleep: func(time.Duration) {},
	}
}

func TestNextOrdersAndExplains(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"ready --json":                     []byte(readyJSON),
		"list --status in_progress --json": []byte(inProgressJSON),
	}}
	out, err := intent.Next(context.Background(), newEnv(f), intent.NextOpts{N: 5})
	if err != nil {
		t.Fatal(err)
	}
	// x-1 beats x-7: same priority, but x-1's parent already has x-9 in flight.
	// x-3 (stale run, warn only) sorts by priority 1 ahead of both.
	got := ids(out)
	if want := "x-3,x-1,x-7"; got != want {
		t.Fatalf("candidates = %s, want %s\n%+v", got, want, out.Candidates)
	}
	if out.ExcludedCount != 5 { // x-2 (W01), x-4 (W03), x-5 (W04), x-6 (W13), x-epic
		t.Errorf("excluded = %d, want 5", out.ExcludedCount)
	}
	if !strings.Contains(strings.Join(out.Candidates[0].Why, ";"), "W06s (warn)") {
		t.Errorf("x-3 should carry a stale-claim warning: %v", out.Candidates[0].Why)
	}
}

func TestNextIncludeUnworkableGivesReasons(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"ready --json":                     []byte(readyJSON),
		"list --status in_progress --json": []byte(`[]`),
	}}
	out, err := intent.Next(context.Background(), newEnv(f), intent.NextOpts{N: 10, IncludeUnworkable: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) != 8 {
		t.Fatalf("got %d candidates, want 8", len(out.Candidates))
	}
	reasons := map[string]string{}
	for _, c := range out.Candidates {
		reasons[c.ID] = strings.Join(c.WhyNot, ";")
	}
	for id, want := range map[string]string{"x-2": "W01", "x-4": "W03", "x-5": "W04", "x-6": "W13", "x-epic": "epic"} {
		if !strings.Contains(reasons[id], want) {
			t.Errorf("%s why_not = %q, want %s", id, reasons[id], want)
		}
	}
}

func TestNextCapsAndPool(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"ready --json":                     []byte(readyJSON),
		"list --status in_progress --json": []byte(`[]`),
	}}
	env := newEnv(f)
	env.Run.Caps = []string{"gpu"}
	env.Run.Pool = "docs"
	out, _ := intent.Next(context.Background(), env, intent.NextOpts{N: 10})
	got := ids(out)
	// pool=docs excludes unlabelled beads? No: unlabelled beads are default
	// pool and a docs run must not take them (W13). Only x-6 qualifies;
	// x-5 has no pool label so it is excluded despite caps matching.
	if got != "x-6" {
		t.Fatalf("got %s, want x-6", got)
	}
}

func TestLintHoldWindow(t *testing.T) {
	updated := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	held := func(at time.Time) *ledger.Issue {
		return &ledger.Issue{ID: "x-1", Status: "open", AcceptanceCriteria: "ac", Parent: "e",
			Labels: []string{"bops:hold"}, UpdatedAt: at}
	}
	for _, tc := range []struct {
		name string
		i    *ledger.Issue
		now  time.Time
		held bool
	}{
		{"inside the window", held(updated), updated.Add(119 * time.Minute), true},
		{"at the boundary", held(updated), updated.Add(120 * time.Minute), false},
		{"after the window", held(updated), updated.Add(3 * time.Hour), false},
		{"no timestamp stays held", held(time.Time{}), updated, true},
	} {
		r := intent.Lint(tc.i, intent.LintEnv{Now: tc.now})
		got := false
		for _, f := range r.Fails {
			got = got || f.ID == "W12"
		}
		if got != tc.held {
			t.Errorf("%s: W12 failed=%v, want %v (%+v)", tc.name, got, tc.held, r)
		}
	}
}

const heldByRun1 = `[{"id":"x-1","status":"in_progress","assignee":"tester","labels":["run:RUN1"],"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`

func TestNoteWritesViaFileAndHeartbeatsAfter(t *testing.T) {
	text := "did \"the\" thing; $(not run)\n\nNEXT: finish it"
	r := &noteRunner{Fake: bdx.Fake{Responses: map[string][]byte{
		"show x-1 --json": []byte(heldByRun1),
		"heartbeat x-1":   []byte(`{}`),
	}}}
	out, err := intent.Note(context.Background(), noteEnv(r), "x-1", text)
	if err != nil {
		t.Fatal(err)
	}
	if out.CommentID != "c-1" || len(out.Warnings) != 0 {
		t.Fatalf("%+v", out)
	}
	if want := "bops:note run=RUN1\n" + text; r.body != want {
		t.Errorf("comment body = %q, want %q", r.body, want)
	}
	if got := strings.Join(callNames(r.Calls), ","); got != "comments,heartbeat" {
		t.Errorf("call order = %s", got)
	}
	for _, call := range r.Calls {
		if strings.Contains(strings.Join(call, " "), "the") {
			t.Errorf("note text reached a bd argument: %v", call)
		}
	}
	if _, err := os.Stat(r.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file should be removed, stat = %v", err)
	}
}

func TestNoteRefusesWhenRunDoesNotHoldBead(t *testing.T) {
	r := &noteRunner{Fake: bdx.Fake{Responses: map[string][]byte{
		"show x-1 --json": []byte(`[{"id":"x-1","status":"in_progress","assignee":"other","labels":["run:OTHER"],"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`),
	}}}
	out, err := intent.Note(context.Background(), newEnv(r), "x-1", "text\n\nNEXT: x")
	if err != nil || out.Outcome != intent.OutcomeRefused || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("must not write; calls = %v", r.Calls)
	}
	saved, err := os.ReadFile(out.SavedTo)
	if err != nil || string(saved) != "text\n\nNEXT: x" {
		t.Errorf("refused note was not saved: %q, %v", saved, err)
	}
	_ = os.Remove(out.SavedTo)
}

func TestNoteRefusesEmptyText(t *testing.T) {
	r := &noteRunner{}
	if _, err := intent.Note(context.Background(), newEnv(r), "x-1", " \n\t"); !errors.Is(err, intent.ErrPrecondition) {
		t.Fatalf("want ErrPrecondition, got %v", err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("must not call bd; calls = %v", r.Calls)
	}
}

func TestNoteWarnsAboutLastParagraph(t *testing.T) {
	for _, tc := range []struct {
		text string
		warn bool
	}{
		{"done\n\nNEXT: more", false},
		{"done\n\nBLOCKER: stuck", false},
		{"done\n\nKEY DECISION: chose x", false},
		{"done\r\n\r\nREF: url\r\n", false},
		{"NEXT: only one paragraph", false},
		{"NEXT: first\n\nthen prose", true},
		{"next: lower case", true},
		{"just prose", true},
	} {
		r := &noteRunner{Fake: bdx.Fake{Responses: map[string][]byte{
			"show x-1 --json": []byte(heldByRun1),
			"heartbeat x-1":   []byte(`{}`),
		}}}
		out, err := intent.Note(context.Background(), noteEnv(r), "x-1", tc.text)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(strings.Join(out.Warnings, ";"), "W09")
		if got != tc.warn {
			t.Errorf("%q: W09 warning = %v, want %v (%v)", tc.text, got, tc.warn, out.Warnings)
		}
	}
}

func TestNoteSurvivesAFailedHeartbeat(t *testing.T) {
	r := &noteRunner{Fake: bdx.Fake{Responses: map[string][]byte{
		"show x-1 --json": []byte(heldByRun1),
	}}} // no heartbeat response, so the fake fails it
	out, err := intent.Note(context.Background(), noteEnv(r), "x-1", "t\n\nNEXT: x")
	if err != nil {
		t.Fatalf("a saved note must not fail on heartbeat: %v", err)
	}
	if !strings.Contains(strings.Join(out.Warnings, ";"), "heartbeat failed") {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// --- helpers

// noteEnv holds x-1 as a legacy claim, which has no ref to refresh. The ref
// paths are covered in ownership_test.go.
func noteEnv(r bdx.Runner) *intent.Env {
	env := newEnv(r)
	env.Run.Hold("x-1")
	env.Lock = &lock.Lock{Journal: journalOf{env.Run}}
	return env
}

// journalOf reads a Run's journal for tests that make no ref writes.
type journalOf struct{ run *runstate.Run }

func (j journalOf) Entry(id string) (runstate.Entry, bool) { e, ok := j.run.Held[id]; return e, ok }
func (j journalOf) Intend(string, string) error            { return nil }
func (j journalOf) Confirm(string, string) error           { return nil }
func (j journalOf) Abandon(string) error                   { return nil }
func (j journalOf) IntendDelete(string) error              { return nil }
func (j journalOf) Drop(id string) error                   { j.run.Drop(id); return nil }

// noteRunner reads the comment file while bd would, since it is removed after.
type noteRunner struct {
	bdx.Fake
	path, body string
}

func (r *noteRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) >= 4 && args[0] == "comments" && args[1] == "add" && args[3] == "-f" {
		r.Calls = append(r.Calls, args)
		b, err := os.ReadFile(args[4])
		if err != nil {
			return nil, err
		}
		r.path, r.body = args[4], string(b)
		return []byte(`{"id":"c-1"}`), nil
	}
	return r.Fake.Run(ctx, args...)
}

func callNames(calls [][]string) []string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c[0])
	}
	return names
}

type flaky struct {
	bdx.Fake
	failFirst string
	n         *int
}

func (f *flaky) Run(ctx context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if key == f.failFirst {
		*f.n++
		if *f.n == 1 {
			return nil, errors.New("bd dolt push: database is locked by another process")
		}
	}
	return f.Fake.Run(ctx, args...)
}

func TestSyncRetriesOnceOnLock(t *testing.T) {
	calls := 0
	f := &flaky{Fake: bdx.Fake{Responses: map[string][]byte{"dolt pull": []byte(``), "dolt push": []byte(``)}},
		failFirst: "dolt push", n: &calls}
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "A")
	git(t, filepath.Dir(dir), "clone", "-q", remote, dir)
	s := openStore(t, dir, "A")
	env := &intent.Env{BD: f, Run: s.Run(), Store: s, Sleep: func(time.Duration) {},
		Lock: &lock.Lock{Git: lock.Exec{Dir: dir}, Journal: s, Run: "A", Actor: "tester"}}
	out, err := intent.Sync(context.Background(), env, false)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Pulled || !out.Pushed {
		t.Fatalf("%+v", out)
	}
	if calls != 2 {
		t.Errorf("push should have been attempted twice, got %d", calls)
	}
}

func ids(o *intent.NextOut) string {
	var s []string
	for _, c := range o.Candidates {
		s = append(s, c.ID)
	}
	return strings.Join(s, ",")
}
