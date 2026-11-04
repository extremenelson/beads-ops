package intent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
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

func TestClaimHappyPath(t *testing.T) {
	before := `[{"id":"x-1","title":"t","status":"open","priority":2,"issue_type":"task","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
	after := `[{"id":"x-1","title":"t","status":"in_progress","assignee":"tester","labels":["run:RUN1"],"priority":2,"issue_type":"task","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
	f := &bdx.Fake{
		Responses: map[string][]byte{
			"update x-1 --claim --if-assignee ":                               []byte(`{}`),
			"label add x-1 run:RUN1":                                          []byte(``),
			"comments add x-1 bops:claimed run=RUN1 actor=tester machine=box": []byte(``),
			"dolt push": []byte(``),
		},
		// first show = precondition read, second show = readback after the write
		Sequences: map[string][][]byte{"show x-1 --json": {[]byte(before), []byte(after)}},
	}
	env := newEnv(f)
	out, err := intent.Claim(context.Background(), env, "x-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !out.ReadbackOK || out.Run != "RUN1" {
		t.Fatalf("%+v", out)
	}
	if len(env.Run.Held) != 1 || env.Run.Held[0] != "x-1" {
		t.Errorf("run should hold x-1: %v", env.Run.Held)
	}
	if f.Calls[len(f.Calls)-1][0] != "dolt" {
		t.Errorf("last call should be dolt push, got %v", f.Calls[len(f.Calls)-1])
	}
}

func TestClaimRefusesWhenAssigned(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"show x-1 --json": []byte(`[{"id":"x-1","status":"open","assignee":"other","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`),
	}}
	_, err := intent.Claim(context.Background(), newEnv(f), "x-1", false)
	if !errors.Is(err, intent.ErrPrecondition) {
		t.Fatalf("want ErrPrecondition, got %v", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("must not write after a failed precondition; calls=%v", f.Calls)
	}
}

func TestReleaseEffects(t *testing.T) {
	held := `[{"id":"x-1","status":"in_progress","assignee":"tester","labels":["run:RUN1"],"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
	for reason, extra := range map[string]string{
		"blocked": "update x-1 --status blocked",
		"human":   "label add x-1 needs:human",
		"scope":   "label add x-1 bops:hold",
		"done":    "",
	} {
		f := &bdx.Fake{Responses: map[string][]byte{
			"show x-1 --json":           []byte(held),
			"update x-1 --assignee ":    []byte(``),
			"label remove x-1 run:RUN1": []byte(``),
			"comments add x-1 bops:released run=RUN1 reason=" + reason: []byte(``),
			"dolt push": []byte(``),
		}}
		if extra != "" {
			f.Responses[extra] = []byte(``)
		}
		env := newEnv(f)
		env.Run.Hold("x-1")
		if _, err := intent.Release(context.Background(), env, "x-1", reason); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if len(env.Run.Held) != 0 {
			t.Errorf("%s: run should no longer hold x-1", reason)
		}
	}
}

func TestReleaseRefusesOtherRun(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"show x-1 --json": []byte(`[{"id":"x-1","status":"in_progress","assignee":"other","labels":["run:OTHER"],"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`),
	}}
	if _, err := intent.Release(context.Background(), newEnv(f), "x-1", "done"); !errors.Is(err, intent.ErrPrecondition) {
		t.Fatalf("want ErrPrecondition, got %v", err)
	}
}

func TestSyncRetriesOnceOnLock(t *testing.T) {
	calls := 0
	f := &flaky{Fake: bdx.Fake{Responses: map[string][]byte{"dolt pull": []byte(``), "dolt push": []byte(``), "heartbeat x-1": []byte(``)}},
		failFirst: "dolt push", n: &calls}
	env := newEnv(f)
	env.Run.Hold("x-1")
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

// --- helpers

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

func ids(o *intent.NextOut) string {
	var s []string
	for _, c := range o.Candidates {
		s = append(s, c.ID)
	}
	return strings.Join(s, ",")
}
