// Package depgraph answers reachability and longest-path questions over the
// blocking edges of a ledger, in Go, over a graph fetched once (SPEC.md §4,
// bops deps). The longest path is a topological pass (Kahn's algorithm), not
// path enumeration: enumeration does not finish at scale, a topological pass
// takes milliseconds.
package depgraph

import (
	"fmt"
	"sort"
	"strings"
)

// Graph holds beads and "waits on" edges: Wait(a, b) means a cannot start
// until b is done.
type Graph struct {
	blockers   map[string]map[string]bool // id -> what it waits on
	dependents map[string]map[string]bool // id -> what waits on it
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{blockers: map[string]map[string]bool{}, dependents: map[string]map[string]bool{}}
}

// Add records a bead with no edges, if it is not there already.
func (g *Graph) Add(id string) {
	if g.blockers[id] == nil {
		g.blockers[id] = map[string]bool{}
		g.dependents[id] = map[string]bool{}
	}
}

// Has reports whether the bead is in the graph.
func (g *Graph) Has(id string) bool { return g.blockers[id] != nil }

// Wait records that blocked waits on blocker, adding both beads.
func (g *Graph) Wait(blocked, blocker string) {
	g.Add(blocked)
	g.Add(blocker)
	g.blockers[blocked][blocker] = true
	g.dependents[blocker][blocked] = true
}

// Blockers lists what id waits on directly, sorted.
func (g *Graph) Blockers(id string) []string { return keys(g.blockers[id]) }

// Dependents lists what waits on id directly, sorted.
func (g *Graph) Dependents(id string) []string { return keys(g.dependents[id]) }

// Upstream lists everything id waits on, directly or not, sorted. It
// terminates on cycles and never includes id unless id is on one.
func (g *Graph) Upstream(id string) []string { return keys(g.reach(id, g.blockers)) }

// Downstream lists everything that waits on id, directly or not, sorted.
func (g *Graph) Downstream(id string) []string { return keys(g.reach(id, g.dependents)) }

func (g *Graph) reach(id string, next map[string]map[string]bool) map[string]bool {
	seen := map[string]bool{}
	queue := []string{id}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for u := range next[v] {
			if !seen[u] {
				seen[u] = true
				queue = append(queue, u)
			}
		}
	}
	return seen
}

// CycleError names a cycle: each bead waits on the next, and the last on the
// first.
type CycleError struct{ Cycle []string }

func (e *CycleError) Error() string {
	return "dependency cycle (each bead waits on the next): " + strings.Join(append(e.Cycle, e.Cycle[0]), " -> ")
}

// CriticalPath returns the longest chain of beads that ends at id, each one
// waiting on the one before it, so the first bead is where the work starts.
// Among chains of equal length it picks the one whose beads, read back from
// id, sort first. A cycle among the beads id waits on is a *CycleError.
func (g *Graph) CriticalPath(id string) ([]string, error) {
	if !g.Has(id) {
		return []string{id}, nil
	}
	sub := g.reach(id, g.blockers)
	sub[id] = true // also when id waits on itself: Kahn's pass finds that cycle
	// Kahn's algorithm over the beads id waits on, plus id: a bead is ready
	// once everything it waits on in sub is ordered. Sorted for determinism.
	indeg := map[string]int{}
	for v := range sub {
		for u := range g.blockers[v] {
			if sub[u] {
				indeg[v]++
			}
		}
	}
	var ready, order []string
	for v := range sub {
		if indeg[v] == 0 {
			ready = append(ready, v)
		}
	}
	sort.Strings(ready)
	for len(ready) > 0 {
		v := ready[0]
		ready = ready[1:]
		order = append(order, v)
		for _, w := range keys(g.dependents[v]) {
			if !sub[w] {
				continue
			}
			if indeg[w]--; indeg[w] == 0 {
				ready = append(ready, w)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(sub) {
		left := map[string]bool{}
		for v := range sub {
			if indeg[v] > 0 {
				left[v] = true
			}
		}
		return nil, &CycleError{g.cycleIn(left)}
	}
	// Longest chain ending at each bead, in topological order. prev keeps
	// the smallest id among equally long predecessors.
	dist := map[string]int{}
	prev := map[string]string{}
	for _, v := range order {
		for _, u := range keys(g.blockers[v]) {
			if !sub[u] {
				continue
			}
			if d := dist[u] + 1; d > dist[v] {
				dist[v], prev[v] = d, u
			}
		}
	}
	path := []string{id}
	for v := id; prev[v] != ""; v = prev[v] {
		path = append(path, prev[v])
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

// cycleIn finds one cycle among nodes, following waits-on edges inside
// nodes. Every bead in nodes must wait on another bead in nodes, which holds
// for the beads Kahn's pass leaves unordered. It starts from the
// smallest id so the answer is deterministic.
func (g *Graph) cycleIn(nodes map[string]bool) []string {
	start := keys(nodes)[0]
	pos := map[string]int{}
	var walk []string
	for v := start; ; {
		if i, ok := pos[v]; ok {
			return rotate(walk[i:])
		}
		pos[v] = len(walk)
		walk = append(walk, v)
		next := ""
		for _, u := range keys(g.blockers[v]) {
			if nodes[u] {
				next = u
				break
			}
		}
		if next == "" {
			panic(fmt.Sprintf("depgraph: %s has no blocker inside the cyclic set", v))
		}
		v = next
	}
}

// rotate starts a cycle at its smallest id.
func rotate(c []string) []string {
	m := 0
	for i := range c {
		if c[i] < c[m] {
			m = i
		}
	}
	return append(append([]string{}, c[m:]...), c[:m]...)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
