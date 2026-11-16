package intent_test

import (
	"context"
	"errors"
	"fmt"
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

// graph is in the contract's enum but not built: it refuses, and bd is
// never asked.
func TestDepsReservedEngine(t *testing.T) {
	f := &bdx.Fake{}
	_, err := intent.Deps(context.Background(), newEnv(f), "x-1", intent.DepsOpts{Engine: "graph"})
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") || len(f.Calls) != 0 {
		t.Errorf("engine=graph: err %v, calls %v", err, f.Calls)
	}
}

// sqlRunner answers bd sql with canned outputs in order, and records each
// query, so the cte engine's handling of rows is tested without a server.
type sqlRunner struct {
	outs    []string
	err     error
	queries []string
}

func (r *sqlRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) != 3 || args[0] != "sql" || args[1] != "--json" {
		return nil, fmt.Errorf("unexpected bd %v", args)
	}
	r.queries = append(r.queries, args[2])
	if r.err != nil {
		return nil, r.err
	}
	out := r.outs[0]
	if len(r.outs) > 1 {
		r.outs = r.outs[1:]
	}
	return []byte(out), nil
}

// The rows the server returns for x-5 in depsLedger: unsorted, counts as
// numbers. dist is the longest chain ending at each bead x-5 waits on, and
// x-5 itself; edge rows are the wait edges among them. x-4 has two blockers
// of chain length 1 or less: x-3 (1) and x-2 (0), and only x-3 is one
// shorter than x-4.
const cteX5 = `[
 {"kind":"status","a":"open","b":"","n":0},
 {"kind":"dist","a":"x-5","b":"","n":3},{"kind":"dist","a":"x-4","b":"","n":2},{"kind":"dist","a":"x-3","b":"","n":1},
 {"kind":"dist","a":"x-2","b":"","n":0},{"kind":"dist","a":"x-1","b":"","n":0},{"kind":"dist","a":"g-1","b":"","n":0},
 {"kind":"dist","a":"w-hidden","b":"","n":0},
 {"kind":"blocked_by","a":"x-4","b":"","n":0},
 {"kind":"blocked_by","a":"w-hidden","b":"","n":0},
 {"kind":"blocked_by","a":"g-1","b":"","n":0},
 {"kind":"edge","a":"x-5","b":"x-4","n":0},{"kind":"edge","a":"x-5","b":"g-1","n":0},{"kind":"edge","a":"x-5","b":"w-hidden","n":0},
 {"kind":"edge","a":"x-4","b":"x-3","n":0},{"kind":"edge","a":"x-4","b":"x-2","n":0},
 {"kind":"edge","a":"x-3","b":"x-1","n":0}
]`

func TestDepsCTEReadsTheRows(t *testing.T) {
	r := &sqlRunner{outs: []string{cteX5}}
	out, err := intent.Deps(context.Background(), newEnv(r), "x-5", intent.DepsOpts{Engine: "cte", Transitive: true, Path: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out.BlockedBy, ","); got != "g-1,w-hidden,x-1,x-2,x-3,x-4" {
		t.Errorf("blocked_by = %s", got)
	}
	if got := strings.Join(out.CriticalPath, ","); got != "x-1,x-3,x-4,x-5" || out.Depth != 3 {
		t.Errorf("critical_path = %s, depth %d", got, out.Depth)
	}
	if len(out.Blocks) != 0 || out.Blocks == nil {
		t.Errorf("blocks = %#v, want empty, not null", out.Blocks)
	}
	q := r.queries[0]
	for _, want := range []string{"with recursive", "'x-5'", "'edge'", "'dist'", "'down'"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s:\n%s", want, q)
		}
	}
	// Without --transitive and --path the query asks for neither.
	r = &sqlRunner{outs: []string{cteX5}}
	out, _ = intent.Deps(context.Background(), newEnv(r), "x-5", intent.DepsOpts{Engine: "cte"})
	if got := strings.Join(out.BlockedBy, ","); got != "g-1,w-hidden,x-4" || out.CriticalPath != nil || out.Depth != 3 {
		t.Errorf("direct: %+v", out)
	}
	if strings.Contains(r.queries[0], "'edge'") || strings.Contains(r.queries[0], "'down'") {
		t.Errorf("direct query asks for the closure or the path:\n%s", r.queries[0])
	}
}

func TestDepsCTEClosedUnknownAndEmbedded(t *testing.T) {
	ctx := context.Background()
	closed := &sqlRunner{outs: []string{`[{"kind":"status","a":"closed","b":"","n":0},{"kind":"blocked_by","a":"x-9","b":"","n":0}]`}}
	out, err := intent.Deps(ctx, newEnv(closed), "x-old", intent.DepsOpts{Engine: "cte", Path: true})
	if err != nil || len(out.Blocks)+len(out.BlockedBy) != 0 || strings.Join(out.CriticalPath, ",") != "x-old" {
		t.Errorf("closed: %+v, %v", out, err)
	}
	if _, err := intent.Deps(ctx, newEnv(&sqlRunner{outs: []string{`[]`}}), "x-nope", intent.DepsOpts{Engine: "cte"}); err == nil || !strings.Contains(err.Error(), "no issue x-nope") {
		t.Errorf("unknown: %v", err)
	}
	emb := &sqlRunner{err: errors.New("bd sql --json ...: exit status 1: Error: 'bd sql' is not yet supported in embedded mode")}
	if _, err := intent.Deps(ctx, newEnv(emb), "x-1", intent.DepsOpts{Engine: "cte"}); err == nil || !strings.Contains(err.Error(), "needs bd in server mode") {
		t.Errorf("embedded: %v", err)
	}
}

// The id goes into the SQL text, so anything but an id is refused unasked.
func TestDepsCTERefusesWhatIsNotAnID(t *testing.T) {
	for _, id := range []string{"x-1' or '1'='1", "x 1", "", "-x", "x;drop"} {
		r := &sqlRunner{outs: []string{`[]`}}
		if _, err := intent.Deps(context.Background(), newEnv(r), id, intent.DepsOpts{Engine: "cte"}); err == nil || !strings.Contains(err.Error(), "not a bead id") || len(r.queries) != 0 {
			t.Errorf("%q: err %v, queries %d", id, err, len(r.queries))
		}
	}
}

// A chain as long as the closure has beads means a cycle. With --path the
// edges came with the first query; without it a second query fetches them.
// Either way the error names the cycle as engine=bd does.
func TestDepsCTECycle(t *testing.T) {
	dist := `[{"kind":"status","a":"open","b":"","n":0},{"kind":"dist","a":"c-1","b":"","n":3},{"kind":"dist","a":"c-2","b":"","n":3},{"kind":"dist","a":"c-3","b":"","n":3}`
	edges := `{"kind":"edge","a":"c-1","b":"c-2","n":0},{"kind":"edge","a":"c-2","b":"c-3","n":0},{"kind":"edge","a":"c-3","b":"c-1","n":0}]`
	for _, c := range []struct {
		path    bool
		outs    []string
		queries int
	}{
		{false, []string{dist + "]", "[" + edges}, 2},
		{true, []string{dist + "," + edges}, 1},
	} {
		r := &sqlRunner{outs: c.outs}
		_, err := intent.Deps(context.Background(), newEnv(r), "c-2", intent.DepsOpts{Engine: "cte", Path: c.path})
		var ce *depgraph.CycleError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), "c-1 -> c-2 -> c-3 -> c-1") || len(r.queries) != c.queries {
			t.Errorf("path=%v: err = %v, queries %d", c.path, err, len(r.queries))
		}
	}
}

// bd closes the connection when a query outlasts its 10s read timeout; the
// error says so and points at engine=bd.
func TestDepsCTETimeout(t *testing.T) {
	r := &sqlRunner{err: errors.New("bd sql --json ...: exit status 1: query error: invalid connection")}
	if _, err := intent.Deps(context.Background(), newEnv(r), "x-1", intent.DepsOpts{Engine: "cte"}); err == nil || !strings.Contains(err.Error(), "outlasted bd's read timeout") {
		t.Errorf("err = %v", err)
	}
}
