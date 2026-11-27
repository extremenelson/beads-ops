package intent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// OutcomeRenewed is a renew of one claim that refreshed its ref.
const OutcomeRenewed = "renewed"

// RenewBead is what renew did for one bead.
type RenewBead struct {
	ID      string  `json:"id"`
	Outcome string  `json:"outcome"` // renewed | lost | refused | unreachable | error
	Reason  string  `json:"reason,omitempty"`
	Code    string  `json:"code,omitempty"`
	Holder  *Holder `json:"holder,omitempty"`
}

// RenewOut is the `renew` output.
type RenewOut struct {
	Beads    []RenewBead `json:"beads"`
	Warnings []string    `json:"warnings"`
}

// Negative reports a bead this run does not hold, or no longer holds.
func (o *RenewOut) Negative() bool {
	if o == nil {
		return false
	}
	for _, b := range o.Beads {
		if negative(b.Outcome) {
			return true
		}
	}
	return false
}

// CodeLegacyHold marks a renew refused because the hold predates the claim
// lock: the bead is still held, but it has no ref to renew.
const CodeLegacyHold = "legacy_hold"

// Renew is a heartbeat for an orchestrator: for each bead it refreshes this
// run's claim ref, as note does, so the claim's age (stale, reap, claim
// --force) starts again, and it writes nothing to the ledger: no comment, no
// bd heartbeat. It must run as the run that claimed (the same checkout and
// BOPS_RUN). A bead this run does not hold, or an invalid id, is refused, and
// one whose ref another run has taken is lost (the hold is dropped), as for
// note. A bead whose ref could not be refreshed (offline, or another bops
// process on this machine kept the bead busy) keeps its hold and makes the
// run an error, so the caller retries; the other beads are still renewed,
// except that once the remote is unreachable or refuses the credentials, the
// beads after it are not tried. With lock.mode none there is no ref, a claim
// ages from the ledger, and renew refuses to run.
func Renew(ctx context.Context, env *Env, ids []string) (*RenewOut, error) {
	out := &RenewOut{Beads: []RenewBead{}, Warnings: []string{}}
	if len(ids) == 0 {
		return out, fmt.Errorf("%w: renew needs at least one bead id", ErrPrecondition)
	}
	if env.LockOff || env.Lock == nil {
		return out, fmt.Errorf("%w: renew needs the claim lock (lock.mode is none, so a claim ages from the ledger; bops note refreshes it)", ErrPrecondition)
	}
	var uniq []string
	for _, id := range ids {
		if !slices.Contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	var failed []error
	code := ""
	var down *RenewBead // the first bead the remote failed: unreachable, or credentials refused
	for _, id := range uniq {
		var b RenewBead
		switch err := lock.ValidateID(id); {
		case err != nil:
			// One bad id must not stop the heartbeat of the others.
			b = RenewBead{ID: id, Outcome: OutcomeRefused, Reason: lock.Scrub(err.Error()), Code: lock.CodeOf(err)}
		case down != nil:
			// The remote is down: each further bead would wait out its own
			// network timeouts and eat the command's budget for nothing.
			b = RenewBead{ID: id, Outcome: down.Outcome, Code: down.Code,
				Reason: "not tried: the remote failed for " + down.ID}
		default:
			b = env.renewOne(ctx, id, &out.Warnings)
			if b.Code == lock.CodeUnreachable || b.Code == lock.CodeAuth {
				down = &b
			}
		}
		if b.Outcome == OutcomeUnreachable || b.Outcome == OutcomeError {
			failed = append(failed, fmt.Errorf("%s: %s", b.ID, b.Reason))
			code = worseCode(code, b.Code)
		}
		out.Beads = append(out.Beads, b)
	}
	if len(failed) > 0 {
		err := fmt.Errorf("claims not renewed: %w", errors.Join(failed...))
		if code != "" {
			return out, &lock.Error{Code: code, Err: err}
		}
		return out, err
	}
	return out, nil
}

// worseCode picks the code a caller should act on first: credentials, then
// the network, then whatever came first.
func worseCode(have, next string) string {
	rank := func(c string) int {
		switch c {
		case lock.CodeAuth:
			return 3
		case lock.CodeUnreachable:
			return 2
		case "":
			return 0
		}
		return 1
	}
	if rank(next) > rank(have) {
		return next
	}
	return have
}

// renewOne renews one bead under its lock.
func (e *Env) renewOne(ctx context.Context, id string, warnings *[]string) RenewBead {
	b := RenewBead{ID: id}
	fail := func(err error) RenewBead {
		b.Outcome, b.Reason, b.Code = OutcomeError, lock.Scrub(err.Error()), lock.CodeOf(err)
		if b.Code == lock.CodeUnreachable {
			b.Outcome = OutcomeUnreachable
		}
		return b
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("not reached: the command's time ran out: %w", err))
	}
	unlock, held, err := e.lockHeld(ctx, "renew", id, warnings)
	if err != nil {
		return fail(err)
	}
	if !held {
		b.Outcome, b.Reason = OutcomeRefused, id+" is not held by this run"
		return b
	}
	defer unlock()
	switch r := e.refreshRef(ctx, "renew", id, OutcomeRenewed, warnings); r.outcome {
	case refreshDone:
		b.Outcome = OutcomeRenewed
	case refreshLost:
		b.Outcome, b.Reason, b.Holder = OutcomeLost, id+" is no longer held by this run", r.holder
	case refreshFailed:
		return fail(r.err)
	case refreshLegacy:
		b.Outcome, b.Reason, b.Code = OutcomeRefused, id+" was claimed before the claim lock, so it has no ref to renew; bops note keeps it", CodeLegacyHold
	default: // refreshNoRef: Renew refuses lock.mode none before this
		b.Outcome, b.Reason = OutcomeRefused, id+" has no claim ref (lock.mode is none)"
	}
	return b
}
