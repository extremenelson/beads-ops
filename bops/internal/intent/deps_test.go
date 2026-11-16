package intent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/depgraph"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// `bd list --all --limit 0 --json` shape, invented content. x-4 waits on x-3
// (blocks) and x-2 (waits-for); x-3 waits on x-1 (conditional-blocks) and on
// x-old, which is closed; x-2 is the epic's child (parent-child, not a wait)
// and discovered-from x-1 (not a wait); x-5 waits on x-4, on g-1, a gate,
// and on w-hidden, which the list does not show and so counts as open.
const depsLedger = `[
 {"id":"x-epic","title":"epic","status":"open","priority":1,"issue_type":"epic"},
 {"id":"x-old","title":"done","status":"closed","priority":1,"issue_type":"task"},
 {"id":"x-1","title":"one","status":"open","priority":1,"issue_type":"task"},
 {"id":"x-2","title":"two","status":"in_progress","priority":1,"issue_type":"task","dependencies":[
   {"issue_id":"x-2","depends_on_id":"x-epic","type":"parent-child"},
   {"issue_id":"x-2","depends_on_id":"x-1","type":"discovered-from"}]},
 {"id":"x-3","title":"three","status":"open","priority":1,"issue_type":"task","dependencies":[
   {"issue_id":"x-3","depends_on_id":"x-1","type":"conditional-blocks"},
   {"issue_id":"x-3","depends_on_id":"x-old","type":"blocks"}]},
 {"id":"x-4","title":"four","status":"open","priority":1,"issue_type":"task","dependencies":[
   {"issue_id":"x-4","depends_on_id":"x-3","type":"blocks"},
   {"issue_id":"x-4","depends_on_id":"x-2","type":"waits-for"}]},
 {"id":"x-5","title":"five","status":"open","priority":1,"issue_type":"task","dependencies":[
   {"issue_id":"x-5","depends_on_id":"x-4","type":"blocks"},
   {"issue_id":"x-5","depends_on_id":"g-1","type":"blocks"},
   {"issue_id":"x-5","depends_on_id":"w-hidden","type":"blocks"}]},
 {"id":"g-1","title":"Gate: human","status":"open","priority":2,"issue_type":"gate"}
]`

func depsEnv(ledgerJSON string) *intent.Env {
	return newEnv(&bdx.Fake{Responses: map[string][]byte{"list --all --include-gates --limit 0 --json": []byte(ledgerJSON)}})
}

func TestDepsDirectAndTransitive(t *testing.T) {
	ctx := context.Background()
	out, err := intent.Deps(ctx, depsEnv(depsLedger), "x-4", intent.DepsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out.BlockedBy, ","); got != "x-2,x-3" {
		t.Errorf("blocked_by = %s", got)
	}
	if got := strings.Join(out.Blocks, ","); got != "x-5" {
		t.Errorf("blocks = %s", got)
	}
	if out.CriticalPath != nil || out.Depth != 2 { // x-1 -> x-3 -> x-4
		t.Errorf("without --path: critical_path %v, depth %d (want nil, 2)", out.CriticalPath, out.Depth)
	}
	out, err = intent.Deps(ctx, depsEnv(depsLedger), "x-1", intent.DepsOpts{Transitive: true})
	if err != nil {
		t.Fatal(err)
	}
	// x-2 is only discovered-from x-1, so it does not wait on it.
	if got := strings.Join(out.Blocks, ","); got != "x-3,x-4,x-5" {
		t.Errorf("transitive blocks of x-1 = %s", got)
	}
	// The epic is x-2's parent, not its blocker; x-old is closed.
	out, _ = intent.Deps(ctx, depsEnv(depsLedger), "x-5", intent.DepsOpts{Transitive: true, Path: true})
	if got := strings.Join(out.BlockedBy, ","); got != "g-1,w-hidden,x-1,x-2,x-3,x-4" {
		t.Errorf("transitive blocked_by of x-5 = %s", got)
	}
	if got := strings.Join(out.CriticalPath, ","); got != "x-1,x-3,x-4,x-5" || out.Depth != 3 {
		t.Errorf("critical_path = %s, depth %d", got, out.Depth)
	}
}

// A closed bead waits on nothing and holds nothing back; an unknown id is a
// bd error, not an empty answer.
func TestDepsClosedAndUnknown(t *testing.T) {
	ctx := context.Background()
	out, err := intent.Deps(ctx, depsEnv(depsLedger), "x-old", intent.DepsOpts{Transitive: true, Path: true})
	if err != nil || len(out.Blocks)+len(out.BlockedBy) != 0 || out.Depth != 0 || strings.Join(out.CriticalPath, ",") != "x-old" {
		t.Errorf("closed bead: %+v, %v", out, err)
	}
	if _, err := intent.Deps(ctx, depsEnv(depsLedger), "x-nope", intent.DepsOpts{}); err == nil || !strings.Contains(err.Error(), "no issue x-nope") {
		t.Errorf("unknown id: %v", err)
	}
}

func TestDepsCycleIsAnErrorNamingIt(t *testing.T) {
	cyclic := `[
 {"id":"c-1","status":"open","issue_type":"task","dependencies":[{"issue_id":"c-1","depends_on_id":"c-2","type":"blocks"}]},
 {"id":"c-2","status":"open","issue_type":"task","dependencies":[{"issue_id":"c-2","depends_on_id":"c-3","type":"blocks"}]},
 {"id":"c-3","status":"open","issue_type":"task","dependencies":[{"issue_id":"c-3","depends_on_id":"c-1","type":"blocks"}]}
]`
	_, err := intent.Deps(context.Background(), depsEnv(cyclic), "c-2", intent.DepsOpts{})
	var ce *depgraph.CycleError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "c-1 -> c-2 -> c-3 -> c-1") {
		t.Fatalf("err = %v", err)
	}
}

// cte and graph are in the contract's enum but not built: they refuse, and
// bd is never asked.
func TestDepsReservedEngines(t *testing.T) {
	for _, e := range []string{"cte", "graph"} {
		f := &bdx.Fake{}
		_, err := intent.Deps(context.Background(), newEnv(f), "x-1", intent.DepsOpts{Engine: e})
		if err == nil || !strings.Contains(err.Error(), "not yet implemented") || len(f.Calls) != 0 {
			t.Errorf("engine=%s: err %v, calls %v", e, err, f.Calls)
		}
	}
}
