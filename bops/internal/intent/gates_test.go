package intent_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// Shapes from bd 1.3.4 (`bd gate list --json`, `bd dep list --json`,
// `bd blocked --json`); contents invented.
const gateListX1 = `[
 {"id":"g-h","title":"Gate: human","status":"open","priority":2,"issue_type":"gate","await_type":"human","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"},
 {"id":"g-t","title":"Gate: timer","status":"closed","priority":2,"issue_type":"gate","await_type":"timer","timeout":7200000000000,"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"},
 {"id":"g-p","title":"Gate: gh:pr","status":"open","priority":2,"issue_type":"gate","await_type":"gh:pr","await_id":"42","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"}
]`

const depListX1 = `[
 {"id":"g-h","status":"open","issue_type":"gate","dependency_type":"blocks"},
 {"id":"x-b","status":"open","issue_type":"task","dependency_type":"blocks"},
 {"id":"x-a","status":"in_progress","issue_type":"task","dependency_type":"blocks"},
 {"id":"x-c","status":"closed","issue_type":"task","dependency_type":"blocks"},
 {"id":"x-epic","status":"open","issue_type":"epic","dependency_type":"parent-child"}
]`

func TestGatesListsEveryGateAndTheOpenBlockers(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"gate list x-1 --all --json": []byte(gateListX1),
		"dep list x-1 --json":        []byte(depListX1),
	}}
	out, err := intent.Gates(context.Background(), newEnv(f), "x-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []intent.Gate{
		{ID: "g-h", Type: "human", On: "", Satisfied: false},
		{ID: "g-t", Type: "timer", On: "2h0m0s", Satisfied: true},
		{ID: "g-p", Type: "gh:pr", On: "42", Satisfied: false},
	}
	if !reflect.DeepEqual(out.Gates, want) {
		t.Errorf("gates = %+v\nwant %+v", out.Gates, want)
	}
	// Gates are reported above, closed deps no longer block, and
	// parent-child is not a blocking edge.
	if got := strings.Join(out.BlockingDeps, ","); got != "x-a,x-b" {
		t.Errorf("blocking_deps = %s, want x-a,x-b", got)
	}
}

func TestGatesOnABeadWithNoneIsEmptyNotNull(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"gate list x-1 --all --json": []byte(`[]`),
		"dep list x-1 --json":        []byte(`[]`),
	}}
	out, err := intent.Gates(context.Background(), newEnv(f), "x-1")
	if err != nil || out.Gates == nil || out.BlockingDeps == nil {
		t.Fatalf("out %+v, err %v", out, err)
	}
}

func TestGatesFailsWhenBdDoes(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{}} // no response: the run fails
	if _, err := intent.Gates(context.Background(), newEnv(f), "x-nope"); err == nil {
		t.Fatal("want an error")
	}
}

func TestOpenGatesOfListsOpenGateIDs(t *testing.T) {
	f := &bdx.Fake{Responses: map[string][]byte{
		"gate list x-1 --json": []byte(`[{"id":"g-p","status":"open","issue_type":"gate"},{"id":"g-h","status":"open","issue_type":"gate"}]`),
	}}
	got, err := intent.OpenGatesOf(context.Background(), newEnv(f), "x-1")
	if err != nil || strings.Join(got, ",") != "g-h,g-p" {
		t.Fatalf("got %v, err %v", got, err)
	}
}

// x-g is held back by an open gate alone. x-d also waits on a task, x-ge is an
// epic and x-gi is in progress, so none of those three is listed.
const openGates = `[
 {"id":"g-1","status":"open","issue_type":"gate","await_type":"human"},
 {"id":"g-2","status":"open","issue_type":"gate","await_type":"human"}
]`

const blockedJSON = `[
 {"id":"x-g","title":"gated","status":"open","priority":1,"issue_type":"task","blocked_by":["g-1"],"blocked_by_count":1},
 {"id":"x-d","title":"gate and a task","status":"open","priority":1,"issue_type":"task","blocked_by":["g-2","x-1"],"blocked_by_count":2},
 {"id":"x-ge","title":"gated epic","status":"open","priority":1,"issue_type":"epic","blocked_by":["g-1"],"blocked_by_count":1},
 {"id":"x-gi","title":"gated, in progress","status":"in_progress","priority":1,"issue_type":"task","blocked_by":["g-1"],"blocked_by_count":1}
]`

const showXG = `[{"id":"x-g","title":"gated","description":"d","acceptance_criteria":"ac","status":"open","priority":1,"issue_type":"task","owner":"o","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`

func gatedFake() *bdx.Fake {
	return &bdx.Fake{Responses: map[string][]byte{
		"ready --json":                     []byte(readyJSON),
		"list --status in_progress --json": []byte(`[]`),
		"gate list --limit 0 --json":       []byte(openGates),
		"blocked --json":                   []byte(blockedJSON),
		"show x-g --json":                  []byte(showXG),
	}}
}

// bd ready never returns a gated bead, so with --include-unworkable next
// looks them up and explains them with W05; they never count as excluded
// from bd ready.
func TestNextIncludeUnworkableListsGatedBeadsWithW05(t *testing.T) {
	out, err := intent.Next(context.Background(), newEnv(gatedFake()), intent.NextOpts{N: 20, IncludeUnworkable: true})
	if err != nil {
		t.Fatal(err)
	}
	var g *intent.Candidate
	for i, c := range out.Candidates {
		switch c.ID {
		case "x-g":
			g = &out.Candidates[i]
		case "x-d", "x-ge", "x-gi":
			t.Errorf("%s listed: only beads held back by gates alone are", c.ID)
		}
	}
	if g == nil {
		t.Fatalf("x-g not listed: %s", ids(out))
	}
	if g.Workable || !strings.Contains(strings.Join(g.WhyNot, ";"), "W05: open gates: g-1") || strings.Join(g.BlockedByGates, ",") != "g-1" {
		t.Errorf("x-g = %+v", *g)
	}
	if out.ExcludedCount != 6 { // the same six bd-ready beads as without gates
		t.Errorf("excluded = %d, want 6", out.ExcludedCount)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// Without --include-unworkable next asks bd for nothing more than before.
func TestNextLooksForGatedBeadsOnlyWhenAsked(t *testing.T) {
	f := gatedFake()
	out, err := intent.Next(context.Background(), newEnv(f), intent.NextOpts{N: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if c[0] == "gate" || c[0] == "blocked" || c[0] == "show" {
			t.Errorf("default next ran bd %v", c)
		}
	}
	if strings.Contains(ids(out), "x-g") {
		t.Errorf("x-g listed without --include-unworkable")
	}
}

// A failed gate lookup costs only the gated beads, with a warning.
func TestNextWarnsWhenTheGateLookupFails(t *testing.T) {
	f := gatedFake()
	delete(f.Responses, "blocked --json")
	out, err := intent.Next(context.Background(), newEnv(f), intent.NextOpts{N: 20, IncludeUnworkable: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) == 0 || !strings.Contains(strings.Join(out.Warnings, ";"), "gated beads not listed") {
		t.Errorf("%+v", out)
	}
}
