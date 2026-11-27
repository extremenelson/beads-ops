package intent_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// touchRows is a bd ready reply of workable beads in order; each row is an
// id followed by its touches: paths.
func touchRows(rows ...[]string) []byte {
	var out []string
	for k, r := range rows {
		labels := []string{}
		for _, p := range r[1:] {
			labels = append(labels, fmt.Sprintf("%q", "touches:"+p))
		}
		out = append(out, fmt.Sprintf(`{"id":%q,"title":"t","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","labels":[%s],"created_at":"2026-10-%02dT00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`, r[0], strings.Join(labels, ","), k+1))
	}
	return []byte("[" + strings.Join(out, ",") + "]")
}

func candidate(out *intent.NextOut, id string) *intent.Candidate {
	for k := range out.Candidates {
		if out.Candidates[k].ID == id {
			return &out.Candidates[k]
		}
	}
	return nil
}

// A claims x-busy, which declares bops/internal/intent/. With
// --avoid-conflicts, B's next leaves out the beads whose paths overlap it,
// and keeps the ones that declare other paths or none.
func TestNextAvoidConflicts(t *testing.T) {
	remote := bareRemote(t)
	bd := newBD()
	bd.add("x-busy").Labels = []string{"touches:bops/internal/intent/"}
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-busy")
	b := client(t, remote, "B", bd)
	bd.ready = touchRows(
		[]string{"x-1", "bops/internal/intent/intent.go"},
		[]string{"x-2", "docs/PROCESS.md"},
		[]string{"x-3"},
		[]string{"x-4", "docs/", "bops/*.go"},
	)

	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 10, IncludeUnworkable: true, AvoidConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); !slices.Equal(got, []string{"x-2", "x-3"}) {
		t.Fatalf("workable = %v, want [x-2 x-3]", got)
	}
	if out.ExcludedCount != 2 {
		t.Errorf("excluded_count = %d, want 2", out.ExcludedCount)
	}
	if c := candidate(out, "x-1"); c == nil || !slices.Contains(c.WhyNot, "touches: bops/internal/intent/intent.go overlaps bops/internal/intent/ of claimed x-busy") {
		t.Errorf("x-1: %+v", c)
	}
	if c := candidate(out, "x-4"); c == nil || !slices.Contains(c.WhyNot, "touches: bops/*.go overlaps bops/internal/intent/ of claimed x-busy") {
		t.Errorf("x-4: %+v", c)
	}
	if c := candidate(out, "x-2"); c == nil || !slices.Equal(c.Touches, []string{"docs/PROCESS.md"}) {
		t.Errorf("x-2: %+v, want its touches shown", c)
	}

	// Without the flag the paths are only shown.
	out, err = intent.Next(bg, b.env, intent.NextOpts{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); !slices.Equal(got, []string{"x-1", "x-2", "x-3", "x-4"}) {
		t.Fatalf("workable without --avoid-conflicts = %v", got)
	}
	if c := candidate(out, "x-4"); !slices.Equal(c.Touches, []string{"docs/", "bops/*.go"}) {
		t.Errorf("x-4 touches = %v", c.Touches)
	}
}

// The claim's paths are read even when B's ledger still shows the claimed
// bead open: it is in B's ready list with its labels, and its ref is held.
func TestNextAvoidConflictsWithAClaimTheLedgerHasNotCaughtUpWith(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1").Labels = []string{"touches:docs/PROCESS.md"}
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	bdB.ready = touchRows([]string{"x-1", "docs/PROCESS.md"}, []string{"x-2", "docs/"}, []string{"x-3", "bops/"})

	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 10, IncludeUnworkable: true, AvoidConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); !slices.Equal(got, []string{"x-3"}) {
		t.Fatalf("workable = %v, want [x-3]", got)
	}
	if c := candidate(out, "x-2"); c == nil || !slices.Contains(c.WhyNot, "touches: docs/ overlaps docs/PROCESS.md of claimed x-1") {
		t.Errorf("x-2: %+v", c)
	}
}

// Nothing in progress declares a path, so nothing conflicts.
func TestNextAvoidConflictsWithNoDeclaredClaims(t *testing.T) {
	remote := bareRemote(t)
	bd := newBD()
	bd.add("x-busy")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-busy")
	bd.ready = touchRows([]string{"x-1", "bops/"}, []string{"x-2", "*"})
	out, err := intent.Next(bg, a.env, intent.NextOpts{N: 10, AvoidConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); !slices.Equal(got, []string{"x-1", "x-2"}) {
		t.Fatalf("workable = %v", got)
	}
}

// When the in-progress list cannot be read, the screen still uses the claim
// refs and says it saw only those.
func TestNextAvoidConflictsWarnsWhenTheListFails(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1").Labels = []string{"touches:docs/"}
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	bdB.ready = touchRows([]string{"x-1", "docs/"}, []string{"x-2", "docs/a.md"})
	bdB.fail = func(args []string) error {
		if args[0] == "list" {
			return fmt.Errorf("list down")
		}
		return nil
	}
	out, err := intent.Next(bg, b.env, intent.NextOpts{N: 10, AvoidConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := workable(out); len(got) != 0 {
		t.Errorf("workable = %v, want none", got)
	}
	if !strings.Contains(strings.Join(out.Warnings, ";"), "conflict screening sees only claimed beads bd ready still lists") {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// status shows a claim's declared paths.
func TestStatusShowsTouches(t *testing.T) {
	a, bd, _ := staleWorld(t)
	bd.beads["x-live"].Labels = append(bd.beads["x-live"].Labels, "touches:bops/cmd/", "touches:docs/PROCESS.md")
	out, err := intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := statusByID(out)
	if got := m["x-live"].Touches; !slices.Equal(got, []string{"bops/cmd/", "docs/PROCESS.md"}) {
		t.Errorf("x-live touches = %v", got)
	}
	if got := m["x-here"].Touches; got != nil {
		t.Errorf("x-here touches = %v, want none", got)
	}
}
