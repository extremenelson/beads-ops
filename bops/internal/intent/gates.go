package intent

import (
	"context"
	"sort"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
)

// Gate is one gate on a bead: an issue of type gate that blocks it until it
// is resolved (closed).
type Gate struct {
	ID        string `json:"id"`
	Type      string `json:"type"`      // await_type: human, timer, gh:run, gh:pr, bead
	On        string `json:"on"`        // await_id, or the timer's timeout; "" for human
	Satisfied bool   `json:"satisfied"` // the gate is closed
}

// GatesOut is the result of `bops gates <id>`.
type GatesOut struct {
	Gates        []Gate   `json:"gates"`
	BlockingDeps []string `json:"blocking_deps"`
}

// Gates reports what stands between a bead and bd ready: every gate on it,
// open or resolved, and its open blocking dependencies that are not gates.
func Gates(ctx context.Context, env *Env, id string) (*GatesOut, error) {
	raw, err := env.BD.Run(ctx, "gate", "list", id, "--all", "--json")
	if err != nil {
		return nil, err
	}
	gates, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	out := &GatesOut{Gates: []Gate{}, BlockingDeps: []string{}}
	for _, g := range gates {
		out.Gates = append(out.Gates, Gate{ID: g.ID, Type: g.AwaitType, On: gateOn(g), Satisfied: g.Status == "closed"})
	}
	if raw, err = env.BD.Run(ctx, "dep", "list", id, "--json"); err != nil {
		return nil, err
	}
	deps, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	// An open parent does not hold its children out of bd ready (the children
	// of an open epic are served), so parent-child is left out here although
	// IsBlocking counts it.
	for _, d := range deps {
		if d.IssueType != "gate" && d.Status != "closed" && d.DependencyType != "parent-child" &&
			(ledger.Dependency{Type: d.DependencyType}).IsBlocking() {
			out.BlockingDeps = append(out.BlockingDeps, d.ID)
		}
	}
	sort.Strings(out.BlockingDeps)
	return out, nil
}

func gateOn(g ledger.Issue) string {
	switch {
	case g.AwaitID != "":
		return g.AwaitID
	case g.Timeout > 0:
		return time.Duration(g.Timeout).String()
	}
	return ""
}

// OpenGatesOf lists the ids of a bead's open gates, for W05 in lint.
func OpenGatesOf(ctx context.Context, env *Env, id string) ([]string, error) {
	raw, err := env.BD.Run(ctx, "gate", "list", id, "--json")
	if err != nil {
		return nil, err
	}
	gates, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(gates))
	for _, g := range gates {
		ids = append(ids, g.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// gatedBeads finds the open beads that open gates alone keep out of bd
// ready: every open blocker is a gate. Epics are left out, as next leaves
// them out of its candidates. bd ready never returns these beads, so next
// lists them only with --include-unworkable, to say why (W05). It returns
// each bead, read with bd show so lint sees all of it, and its gate ids.
func gatedBeads(ctx context.Context, env *Env) ([]ledger.Issue, map[string][]string, error) {
	raw, err := env.BD.Run(ctx, "gate", "list", "--limit", "0", "--json")
	if err != nil {
		return nil, nil, err
	}
	gates, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, nil, err
	}
	open := map[string]bool{}
	for _, g := range gates {
		open[g.ID] = true
	}
	if len(open) == 0 {
		return nil, nil, nil
	}
	if raw, err = env.BD.Run(ctx, "blocked", "--json"); err != nil {
		return nil, nil, err
	}
	blocked, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, nil, err
	}
	var beads []ledger.Issue
	byBead := map[string][]string{}
	for _, b := range blocked {
		if b.Status != "open" || b.IssueType == "gate" || b.IssueType == "epic" || len(b.BlockedBy) == 0 {
			continue
		}
		onlyGates := true
		for _, by := range b.BlockedBy {
			onlyGates = onlyGates && open[by]
		}
		if !onlyGates {
			continue
		}
		raw, err := env.BD.Run(ctx, "show", b.ID, "--json")
		if err != nil {
			return nil, nil, err
		}
		full, err := ledger.DecodeIssue(raw)
		if err != nil {
			return nil, nil, err
		}
		ids := append([]string(nil), b.BlockedBy...)
		sort.Strings(ids)
		beads = append(beads, *full)
		byBead[b.ID] = ids
	}
	return beads, byBead, nil
}
