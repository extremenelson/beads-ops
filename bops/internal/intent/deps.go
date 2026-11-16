package intent

import (
	"context"
	"fmt"

	"github.com/extremenelson/beads-ops/bops/internal/depgraph"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
)

// DepsOpts are the `deps` arguments.
type DepsOpts struct {
	Transitive bool
	Path       bool
	Engine     string // bd; cte and graph are reserved
}

// DepsOut is the result of `bops deps <id>`.
type DepsOut struct {
	Blocks       []string `json:"blocks"`
	BlockedBy    []string `json:"blocked_by"`
	CriticalPath []string `json:"critical_path"`
	Depth        int      `json:"depth"`
}

// waitEdges are the dependency types that hold a bead out of bd ready.
// parent-child is not one: an open epic's children are served.
var waitEdges = map[string]bool{"blocks": true, "conditional-blocks": true, "waits-for": true}

// Deps reports what a bead waits on (blocked_by) and what waits on it
// (blocks), directly or with Transitive the whole closure, and depth, the
// length in edges of the longest chain of open beads ending at it; Path adds
// that chain. The graph is every bead that is not closed, from one
// `bd list --all --include-gates --limit 0 --json` (bd hides gates from list
// otherwise), so a closed blocker no longer counts. A cycle among the beads it
// waits on is an error naming the cycle.
func Deps(ctx context.Context, env *Env, id string, o DepsOpts) (*DepsOut, error) {
	switch o.Engine {
	case "", "bd":
	case "cte", "graph":
		return nil, fmt.Errorf("deps: engine=%s is reserved in contract 1 but not yet implemented; use engine=bd", o.Engine)
	default:
		return nil, fmt.Errorf("deps: unknown engine %q", o.Engine)
	}
	raw, err := env.BD.Run(ctx, "list", "--all", "--include-gates", "--limit", "0", "--json")
	if err != nil {
		return nil, err
	}
	issues, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	g, found := waitGraph(issues, id)
	if !found {
		return nil, &ledger.ErrBD{Msg: "no issue " + id}
	}
	out := &DepsOut{Blocks: g.Dependents(id), BlockedBy: g.Blockers(id)}
	if o.Transitive {
		out.Blocks, out.BlockedBy = g.Downstream(id), g.Upstream(id)
	}
	path, err := g.CriticalPath(id)
	if err != nil {
		return nil, err
	}
	out.Depth = len(path) - 1
	if o.Path {
		out.CriticalPath = path
	}
	return out, nil
}

// waitGraph builds the graph of open beads and their wait edges, and says
// whether id is in the ledger at all. A closed id is kept, with no edges. A
// blocker the list does not show (bd hides some types from list) counts as
// open, as bd ready would count it: only a blocker bd reports closed drops out.
func waitGraph(issues []ledger.Issue, id string) (*depgraph.Graph, bool) {
	closed := map[string]bool{}
	found := false
	for _, i := range issues {
		found = found || i.ID == id
		if i.Status == "closed" {
			closed[i.ID] = true
		}
	}
	g := depgraph.New()
	if found {
		g.Add(id)
	}
	for _, i := range issues {
		if closed[i.ID] {
			continue
		}
		g.Add(i.ID)
		for _, d := range i.Dependencies {
			if waitEdges[d.Type] && !closed[d.DependsOnID] {
				g.Wait(i.ID, d.DependsOnID)
			}
		}
	}
	return g, found
}
