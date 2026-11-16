package depgraph

import "github.com/extremenelson/beads-ops/bops/internal/depgraph/depgraphtest"

// genThousand builds the shared 1k-node fixture (depgraphtest.Thousand) as a
// Graph, with its planted longest chain.
func genThousand() (*Graph, []string) {
	ids, edges, chain := depgraphtest.Thousand("b")
	g := New()
	for _, id := range ids {
		g.Add(id)
	}
	for _, e := range edges {
		g.Wait(e.Blocked, e.Blocker)
	}
	return g, chain
}
