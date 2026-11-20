package intent

import (
	"context"
	"errors"
	"strings"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
)

// UnmatchedBlockingLabels returns the configured blocking labels that no bead
// in the ledger carries, closed beads and gates included. Such a label keeps
// nothing out of the pool and says so nowhere: a typo such as forzen, or a
// label bd stores in another form, fails open (bops-jmp). It lists every bead
// once and matches each label exactly, as W15, next and claim do; bd's own
// label filter would parse the value as comma-separated text and could find
// beads for a label those commands never match. A list bd cannot give is an
// error, so the caller can say the check did not run.
func UnmatchedBlockingLabels(ctx context.Context, bd bdx.Runner, labels []string) ([]string, error) {
	out := []string{}
	if len(labels) == 0 {
		return out, nil
	}
	raw, err := bd.Run(ctx, "list", "--all", "--include-gates", "--limit", "0", "--json")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, errors.New("bd list printed nothing")
	}
	issues, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		found := false
		for i := range issues {
			if blockingLabel(&issues[i], []string{l}) != "" {
				found = true
				break
			}
		}
		if !found {
			out = append(out, l)
		}
	}
	return out, nil
}
