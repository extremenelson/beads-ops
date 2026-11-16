package depgraph

import (
	"fmt"
	"math/rand"
)

// genThousand is the synthetic 1k-node DAG fixture, kept as code rather
// than a file. Beads b0000..b0999 sit in 50 layers of 20; a bead waits only
// on beads in earlier layers, so the longest chain has at most 50 beads.
// One bead per layer is chained to the next (the planted chain), so the
// longest chain is exactly 50 and the planted one ends at its last bead.
// Every other edge is random, so chains of 50 may also exist through other
// beads; ties are broken by the beads' ids, and the planted chain uses the
// smallest id in each layer, so it is the one CriticalPath must return.
func genThousand() (*Graph, []string) {
	const layers, width = 50, 20
	id := func(layer, k int) string { return fmt.Sprintf("b%04d", layer*width+k) }
	r := rand.New(rand.NewSource(1))
	g := New()
	var chain []string
	for l := 0; l < layers; l++ {
		for k := 0; k < width; k++ {
			g.Add(id(l, k))
			if l == 0 {
				continue
			}
			for e := 0; e < 3; e++ { // three random blockers from earlier layers
				g.Wait(id(l, k), id(r.Intn(l), r.Intn(width)))
			}
		}
		if l > 0 {
			g.Wait(id(l, 0), id(l-1, 0))
		}
		chain = append(chain, id(l, 0))
	}
	return g, chain
}
