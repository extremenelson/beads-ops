package intent_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

const allBeads = "list --all --include-gates --limit 0 --json"

// UnmatchedBlockingLabels matches labels exactly, as W15, next and claim do,
// against every bead, closed ones and gates included, from one list. A list
// bd cannot give is an error, never a quiet "all matched" (bops-jmp).
func TestUnmatchedBlockingLabels(t *testing.T) {
	beads := `[{"id":"x-1","status":"closed","labels":["frozen"]},` +
		`{"id":"x-2","status":"open","issue_type":"gate","labels":["gated"]},` +
		`{"id":"x-3","status":"open","labels":["Okta"]}]`
	for _, c := range []struct {
		name, reply string
		labels      []string
		want        []string
		wantErr     bool
	}{
		{"closed and gate beads count", beads, []string{"frozen", "gated"}, []string{}, false},
		{"a typo is named", beads, []string{"forzen", "frozen"}, []string{"forzen"}, false},
		{"the match is exact", beads, []string{"okta", `"frozen"`, "Okta"}, []string{`"frozen"`, "okta"}, false},
		{"an empty ledger matches nothing", `[]`, []string{"frozen"}, []string{"frozen"}, false},
		{"an error reply is an error", `{"error":"database locked"}`, []string{"frozen"}, nil, true},
		{"an empty reply is an error", ``, []string{"frozen"}, nil, true},
		{"no reply is an error", "-", []string{"frozen"}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &bdx.Fake{Responses: map[string][]byte{}}
			if c.reply != "-" {
				f.Responses[allBeads] = []byte(c.reply)
			}
			got, err := intent.UnmatchedBlockingLabels(context.Background(), f, c.labels)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if !c.wantErr {
				slices.Sort(got)
				if !slices.Equal(got, c.want) {
					t.Fatalf("got %q, want %q", got, c.want)
				}
			}
			if n := len(f.Calls); n != 1 || strings.Join(f.Calls[0], " ") != allBeads {
				t.Fatalf("want one full list, got %v", f.Calls)
			}
		})
	}
}

// With no blocking labels, bd is not asked.
func TestUnmatchedBlockingLabelsAsksNothingForNoLabels(t *testing.T) {
	f := &bdx.Fake{}
	if got, err := intent.UnmatchedBlockingLabels(context.Background(), f, nil); err != nil || len(got) != 0 || len(f.Calls) != 0 {
		t.Fatalf("got %v, %v, calls %v", got, err, f.Calls)
	}
}
