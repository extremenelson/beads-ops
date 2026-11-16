// Package depgraphtest holds the synthetic graphs that the depgraph tests and
// the bops deps engine tests share, kept as code rather than files.
package depgraphtest

import (
	"fmt"
	"math/rand"
)

// Edge says Blocked waits on Blocker.
type Edge struct{ Blocked, Blocker string }

// Thousand is the 1k-node DAG fixture. Beads <prefix>0000..<prefix>0999 sit
// in 50 layers of 20; a bead waits only on beads in earlier layers, so the
// longest chain has at most 50 beads. One bead per layer is chained to the
// next (the planted chain), so the longest chain is exactly 50 and the
// planted one ends at its last bead. Every other edge is random, so chains of
// 50 may also exist through other beads; ties are broken by the beads' ids,
// and the planted chain uses the smallest id in each layer, so it is the one
// a critical path must return. Edges may repeat; a graph keeps one of each.
func Thousand(prefix string) (ids []string, edges []Edge, chain []string) {
	const layers, width = 50, 20
	id := func(layer, k int) string { return fmt.Sprintf("%s%04d", prefix, layer*width+k) }
	r := rand.New(rand.NewSource(1))
	for l := 0; l < layers; l++ {
		for k := 0; k < width; k++ {
			ids = append(ids, id(l, k))
			if l == 0 {
				continue
			}
			for e := 0; e < 3; e++ { // three random blockers from earlier layers
				edges = append(edges, Edge{id(l, k), id(r.Intn(l), r.Intn(width))})
			}
		}
		if l > 0 {
			edges = append(edges, Edge{id(l, 0), id(l-1, 0)})
		}
		chain = append(chain, id(l, 0))
	}
	return ids, edges, chain
}
