package intent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// maxNextClaims bounds how many candidates claim --next tries. Each lost try
// costs a push, and next has already dropped the beads whose refs are held,
// so more than a few losses in a row means something else is wrong.
const maxNextClaims = 5

// nextBeadWait is how long claim --next waits for a bead another bops process
// on this machine is working on before it moves on: there are other
// candidates, so a long wait only spends the budget.
const nextBeadWait = 2 * time.Second

// nextClaimReserve is the budget a candidate after the first must have left:
// a winning claim still pushes, pulls, writes the ledger and pushes again, and
// one cut short leaves ledger_pending behind.
const nextClaimReserve = 30 * time.Second

// ClaimSkip is one candidate claim --next tried and did not get.
type ClaimSkip struct {
	ID      string  `json:"id"`
	Outcome string  `json:"outcome"`
	Holder  *Holder `json:"holder,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Code    string  `json:"code,omitempty"`
	// RefPending: a claim given back after the pull left its ref; sync deletes it.
	RefPending bool `json:"ref_pending,omitempty"`
}

// ClaimNext claims the first workable bead in next's order. A candidate lost
// or refused, busy in another bops process here (ref_contention), or gone
// from the ledger is listed in skipped and the next one is tried, up to
// maxNextClaims and while nextClaimReserve of the budget is left (checked
// before every try, the first included). An
// unreachable remote, refused credentials or any other failure stops the
// walk, since the next candidate would meet it too. A run that already
// holds a bead is refused before anything is tried.
//
//	next --> claim c1 --skip--> claim c2 ... --> claimed | held
//	  |          |                          \--> refused (tries or budget spent; next_candidate)
//	refused    error (stops)
//	(none workable)
func ClaimNext(ctx context.Context, env *Env) (*ClaimOut, error) {
	// A retry whose first reply was lost must not take a second bead, and a
	// run shared with an orchestrator must not be handed a bead another agent
	// works: a run that already holds any bead is refused, naming them.
	held := make([]string, 0, len(env.Run.Held))
	for id, h := range env.Run.Held {
		if !h.Given() { // released, waiting on its push or ref delete
			held = append(held, id)
		}
	}
	if len(held) > 0 {
		slices.Sort(held)
		return &ClaimOut{Run: env.Run.ID, Outcome: OutcomeRefused, Warnings: []string{},
			Reason: "this run already holds " + strings.Join(held, ", ") + ": work or release it, or claim by id"}, nil
	}
	n, err := Next(ctx, env, NextOpts{N: 1 << 20})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range n.Candidates {
		if c.Workable {
			ids = append(ids, c.ID)
		}
	}
	if env.BeadWait > nextBeadWait {
		saved := env.BeadWait
		env.BeadWait = nextBeadWait
		defer func() { env.BeadWait = saved }()
	}
	warnings := append([]string{}, n.Warnings...)
	addWarnings := func(ws []string) {
		for _, w := range ws {
			if !slices.Contains(warnings, w) {
				warnings = append(warnings, w)
			}
		}
	}
	skipped := []ClaimSkip{}
	finish := func(out *ClaimOut) *ClaimOut {
		out.Warnings = warnings
		if len(skipped) > 0 {
			out.Skipped = skipped
		}
		return out
	}
	var lastErr error // a candidate's own failure
	// bdFailing: every try failed in bd, so bd is failing, not the candidates;
	// that is exit 1, not a verdict.
	bdFailing := func() bool {
		return lastErr != nil && !slices.ContainsFunc(skipped, func(s ClaimSkip) bool { return s.Outcome != OutcomeError || s.Code == lock.CodeContention })
	}
	for k, id := range ids {
		stop := ""
		if k == maxNextClaims {
			stop = fmt.Sprintf("stopped after %d tries", maxNextClaims)
		} else if dl, ok := ctx.Deadline(); ok && time.Until(dl) < nextClaimReserve {
			stop = "stopped: too little of the command's budget left for a claim"
		}
		if stop != "" {
			if bdFailing() {
				return nil, lastErr
			}
			return finish(&ClaimOut{Run: env.Run.ID, Outcome: OutcomeRefused, Reason: stop, NextCandidate: id}), nil
		}
		out, err := claimOne(ctx, env, id, false)
		code := lock.CodeOf(err)
		switch {
		case out == nil && code != lock.CodeUnreachable && code != lock.CodeAuth:
			// The candidate itself failed (gone from the ledger, a bad id).
			skipped = append(skipped, ClaimSkip{ID: id, Outcome: OutcomeError, Reason: err.Error(), Code: code})
			lastErr = err
			continue
		case out == nil:
			return nil, err
		}
		addWarnings(out.Warnings)
		if code == lock.CodeContention {
			skipped = append(skipped, ClaimSkip{ID: id, Outcome: out.Outcome, Reason: err.Error(), Code: code})
			continue
		}
		if err != nil || !out.Negative() {
			return finish(out), err
		}
		skipped = append(skipped, ClaimSkip{ID: id, Outcome: out.Outcome, Holder: out.Holder, Reason: out.Reason, RefPending: out.RefPending})
	}
	if bdFailing() {
		return nil, lastErr
	}
	out := &ClaimOut{Run: env.Run.ID, Outcome: OutcomeRefused}
	if len(ids) == 0 {
		out.Reason = "no workable bead: next lists none"
	} else {
		out.Reason = fmt.Sprintf("no free bead: all %d workable candidates were skipped", len(ids))
	}
	return finish(out), nil
}

// nextCandidate is the first workable bead next offers that is not in skip,
// or "" when there is none or next cannot answer (a warning says why). Next's
// own warnings, such as a failed fetch, are passed on.
func (e *Env) nextCandidate(ctx context.Context, skip map[string]bool, warnings *[]string) string {
	n, err := Next(ctx, e, NextOpts{N: len(skip) + 1})
	if err != nil {
		*warnings = append(*warnings, "next candidate not found: "+err.Error())
		return ""
	}
	for _, w := range n.Warnings {
		if !slices.Contains(*warnings, w) {
			*warnings = append(*warnings, w)
		}
	}
	for _, c := range n.Candidates {
		if c.Workable && !skip[c.ID] {
			return c.ID
		}
	}
	return ""
}
