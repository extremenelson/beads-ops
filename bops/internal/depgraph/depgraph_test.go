package depgraph

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// edges builds a graph from "a<b" pairs: a waits on b.
func edges(pairs ...string) *Graph {
	g := New()
	for _, p := range pairs {
		a, b, _ := strings.Cut(p, "<")
		g.Wait(a, b)
	}
	return g
}

func TestDirectAndTransitive(t *testing.T) {
	// d waits on c and b; c and b wait on a; e waits on d.
	g := edges("d<c", "d<b", "c<a", "b<a", "e<d")
	for _, c := range []struct {
		name string
		got  []string
		want string
	}{
		{"blockers d", g.Blockers("d"), "b,c"},
		{"dependents a", g.Dependents("a"), "b,c"},
		{"upstream d", g.Upstream("d"), "a,b,c"},
		{"downstream a", g.Downstream("a"), "b,c,d,e"},
		{"upstream a", g.Upstream("a"), ""},
		{"unknown", g.Upstream("zz"), ""},
	} {
		if got := strings.Join(c.got, ","); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCriticalPathTieBreaksByID(t *testing.T) {
	g := edges("d<c", "d<b", "c<a", "b<a", "e<d")
	got, err := g.CriticalPath("e")
	if err != nil || strings.Join(got, ",") != "a,b,d,e" {
		t.Fatalf("got %v, %v; want a,b,d,e (b sorts before c)", got, err)
	}
	if got, _ := g.CriticalPath("a"); strings.Join(got, ",") != "a" {
		t.Errorf("a bead with no blockers is its own path: %v", got)
	}
	if got, _ := New().CriticalPath("x"); strings.Join(got, ",") != "x" {
		t.Errorf("a bead not in the graph: %v", got)
	}
}

// A cycle among the beads id waits on is an error that names it; a cycle
// elsewhere is not id's business.
func TestCriticalPathNamesTheCycle(t *testing.T) {
	g := edges("e<d", "d<c", "c<b", "b<d", "b<a", "z<y", "y<z")
	_, err := g.CriticalPath("e")
	var ce *CycleError
	if !errors.As(err, &ce) || strings.Join(ce.Cycle, ",") != "b,d,c" {
		t.Fatalf("err = %v, want the cycle b,d,c", err)
	}
	if !strings.Contains(err.Error(), "b -> d -> c -> b") {
		t.Errorf("message %q", err)
	}
	if _, err := g.CriticalPath("a"); err != nil {
		t.Errorf("a waits on nothing: %v", err)
	}
	if _, err := edges("x<x").CriticalPath("x"); !errors.As(err, &ce) || strings.Join(ce.Cycle, ",") != "x" {
		t.Errorf("self-loop: %v", err)
	}
	if _, err := edges("x<y", "y<x", "y<q").CriticalPath("x"); !errors.As(err, &ce) {
		t.Errorf("id on a cycle with a dead-end blocker: %v", err)
	}
}

// oracle enumerates every chain ending at id by brute force and picks the
// longest; ties go to the chain whose beads, read back from id, sort first.
func oracle(g *Graph, id string) []string {
	var best []string // reversed: id first
	var walk func(path []string)
	walk = func(path []string) {
		better := len(path) > len(best)
		if len(path) == len(best) {
			for i := range path {
				if path[i] != best[i] {
					better = path[i] < best[i]
					break
				}
			}
		}
		if better {
			best = append([]string{}, path...)
		}
		for _, u := range g.Blockers(path[len(path)-1]) {
			walk(append(path, u))
		}
	}
	walk([]string{id})
	out := make([]string, len(best))
	for i := range best {
		out[len(best)-1-i] = best[i]
	}
	return out
}

// randomDAG wires n beads with edges only from a higher index to a lower
// one, so it has no cycle.
func randomDAG(r *rand.Rand, n int, p float64) *Graph {
	g := New()
	for i := 0; i < n; i++ {
		g.Add(fmt.Sprintf("n%02d", i))
		for j := 0; j < i; j++ {
			if r.Float64() < p {
				g.Wait(fmt.Sprintf("n%02d", i), fmt.Sprintf("n%02d", j))
			}
		}
	}
	return g
}

func TestCriticalPathMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for trial := 0; trial < 300; trial++ {
		g := randomDAG(r, 3+r.Intn(10), 0.15+r.Float64()*0.5)
		for id := range g.blockers {
			got, err := g.CriticalPath(id)
			if err != nil {
				t.Fatal(err)
			}
			if want := oracle(g, id); !reflect.DeepEqual(got, want) {
				t.Fatalf("trial %d, %s: got %v, want %v", trial, id, got, want)
			}
		}
	}
}

// bfsOracle is reachability the slow way: repeat until nothing changes.
func bfsOracle(g *Graph, id string, next func(string) []string) []string {
	seen := map[string]bool{}
	frontier := next(id)
	for changed := true; changed; {
		changed = false
		for _, v := range frontier {
			if !seen[v] {
				seen[v], changed = true, true
			}
		}
		frontier = nil
		for v := range seen {
			frontier = append(frontier, next(v)...)
		}
	}
	return keys(seen)
}

// TestThousandNodeDAG runs the generated 1k-node fixture (genThousand):
// closures match the slow oracle, and the critical path is the planted
// chain, the longest the generator allows.
func TestThousandNodeDAG(t *testing.T) {
	g, chain := genThousand()
	if n := len(g.blockers); n != 1000 {
		t.Fatalf("fixture has %d beads", n)
	}
	for _, id := range []string{chain[len(chain)-1], chain[len(chain)/2], "b0499", "b0007", "b0998"} {
		if got, want := g.Upstream(id), bfsOracle(g, id, g.Blockers); !reflect.DeepEqual(got, want) {
			t.Errorf("upstream %s: %d beads, oracle %d", id, len(got), len(want))
		}
		if got, want := g.Downstream(id), bfsOracle(g, id, g.Dependents); !reflect.DeepEqual(got, want) {
			t.Errorf("downstream %s: %d beads, oracle %d", id, len(got), len(want))
		}
	}
	got, err := g.CriticalPath(chain[len(chain)-1])
	if err != nil || !reflect.DeepEqual(got, chain) {
		t.Fatalf("critical path %d long, want the %d-bead planted chain (err %v)", len(got), len(chain), err)
	}
}
