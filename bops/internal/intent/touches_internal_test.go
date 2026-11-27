package intent

import (
	"slices"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

func TestTouchRoot(t *testing.T) {
	for in, want := range map[string]string{
		"bops/intent.go":           "bops/intent.go",
		"./bops/internal/":         "bops/internal",
		"/docs":                    "docs",
		`bops\cmd\main.go`:         "bops/cmd/main.go",
		"bops/internal/*.go":       "bops/internal",
		"bops/**/x.go":             "bops",
		"*":                        ".",
		"a/../b":                   "b",
		"..":                       ".",
		"../other/x":               ".", // out of the repo: everything, not other/x
		"a/../../x":                ".",
		"app/[slug]/page.tsx":      "app",
		"docs/[A-Z]*.md":           "docs",
		"bops/{cmd,internal}/x.go": "bops",
	} {
		if got := touchRoot(in); got != want {
			t.Errorf("touchRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTouchesOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"bops/internal/intent/intent.go", "bops/internal/intent/intent.go", true},
		{"bops/internal/intent/", "bops/internal/intent/status.go", true},
		{"bops/internal/intent/status.go", "bops/internal", true},
		{"bops/*.go", "bops/cmd/bops/main.go", true},
		{"*", "docs/PROCESS.md", true},
		{"docs/PROCESS.md", "docs/COMMANDS.md", false},
		{"bops/internal/intent", "bops/internal/intents", false}, // a segment boundary, not a string prefix
		{"bops/int", "bops/internal/x.go", false},
		{"docs/", "bops/", false},
	} {
		if got := touchesOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("touchesOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := touchesOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("touchesOverlap(%q, %q) = %v, want %v (not symmetric)", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestConflictSkipsTheBeadItselfAndIsStable(t *testing.T) {
	live := map[string][]string{"x-1": {"docs/"}, "x-2": {"docs/a.md"}, "x-3": {"bops/"}}
	order := []string{"x-1", "x-2", "x-3"}
	if got := conflict("x-1", []string{"docs/"}, live, order); got != "touches: docs/ overlaps docs/a.md of claimed x-2" {
		t.Errorf("conflict = %q", got)
	}
	if got := conflict("x-9", []string{"cmd/"}, live, order); got != "" {
		t.Errorf("conflict = %q, want none", got)
	}
	if got := conflict("x-9", nil, live, order); got != "" {
		t.Errorf("a bead with no touches conflicts: %q", got)
	}
}

// Only claims count: an in-progress bead nobody holds does not, a landing one
// does, and a ref counts only while held or landing and only with a bead to
// read its labels from.
func TestLiveTouchesOnlyLiveClaims(t *testing.T) {
	inProgress := []ledger.Issue{
		{ID: "x-bare", Labels: []string{"touches:docs/"}},
		{ID: "x-land", Labels: []string{"bops:landing", "touches:bops/"}},
		{ID: "x-run", Labels: []string{"run:R", "touches:cmd/"}},
		{ID: "x-who", Assignee: "a", Labels: []string{"touches:lib/"}},
		{ID: "x-none", Assignee: "a"},
	}
	ready := []ledger.Issue{
		{ID: "x-held", Labels: []string{"touches:web/"}},
		{ID: "x-gone", Labels: []string{"touches:old/"}},
	}
	refs := map[string]lock.Ref{
		"x-held":  {ID: "x-held", Message: lock.Message{State: lock.StateHeld}},
		"x-gone":  {ID: "x-gone", Message: lock.Message{State: ""}},
		"x-ghost": {ID: "x-ghost", Message: lock.Message{State: lock.StateHeld}}, // in neither list
		"x-bare":  {ID: "x-bare", Message: lock.Message{State: lock.StateLanding}},
	}
	got := liveTouches(inProgress, ready, refs)
	want := map[string][]string{"x-land": {"bops/"}, "x-run": {"cmd/"}, "x-who": {"lib/"}, "x-held": {"web/"}, "x-bare": {"docs/"}}
	if len(got) != len(want) {
		t.Fatalf("live = %v, want %v", got, want)
	}
	for id, w := range want {
		if !slices.Equal(got[id], w) {
			t.Errorf("%s: %v, want %v", id, got[id], w)
		}
	}
	if _, ok := liveTouches(inProgress[:1], nil, nil)["x-bare"]; ok {
		t.Error("an in-progress bead with no claim and no ref counts as live")
	}
}

// An empty touches: label declares nothing; it does not cover the repo.
func TestTouchesDropsEmptyLabels(t *testing.T) {
	i := ledger.Issue{Labels: []string{"touches:", "touches:  ", "touches: docs/ ", "needs:gpu"}}
	if got := touches(&i); !slices.Equal(got, []string{"docs/"}) {
		t.Errorf("touches = %v, want [docs/]", got)
	}
}
