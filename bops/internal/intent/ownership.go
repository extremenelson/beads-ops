package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// The git ref decides who holds a bead. The ledger is a display that catches
// up, so its writes here are best effort. See
// docs/designs/bops-claim-lock-v0.1.0.md.

// Outcomes of a write that uses the lock.
const (
	OutcomeClaimed     = "claimed"
	OutcomeHeld        = "held"
	OutcomeReleased    = "released"
	OutcomeLost        = "lost"
	OutcomeRefused     = "refused"
	OutcomeUnreachable = "unreachable"
	OutcomeError       = "error"
)

// eventDeferred is an events.jsonl outcome only: sync or sweep left a bead
// for the process working on it, or for a later run to read again (bops-yph).
const eventDeferred = "deferred"

const maxPushTries = 5

const (
	retryAdvice       = "retry after 30 seconds, at most 3 times, then stop and report it"
	contentionAdvice  = "another process is updating this bead; retry in a few seconds, at most 3 times, then stop and report it"
	sideEffectsAdvice = "no ledger writes were made; if the ref was written, the local journal records it and the next claim or sync settles it"
)

// Holder says who has a bead, for an agent that lost it.
type Holder struct {
	Actor      string `json:"actor"`
	Run        string `json:"run"`
	AgeMinutes int    `json:"age_minutes"`
}

func negative(outcome string) bool { return outcome == OutcomeLost || outcome == OutcomeRefused }

func (e *Env) holder(r *lock.Ref) *Holder {
	if r == nil {
		return nil
	}
	h := &Holder{Actor: r.Actor, Run: r.Run}
	if !r.At.IsZero() && e.now().After(r.At) {
		h.AgeMinutes = int(e.now().Sub(r.At).Minutes())
	}
	return h
}

func (e *Env) event(cmd, id, step, outcome, sha string, err error) {
	if e.Store == nil {
		return
	}
	ev := runstate.Event{At: e.now(), Run: e.Run.ID, Command: cmd, Bead: id, Step: step, Outcome: outcome, SHA: sha}
	if err != nil {
		ev.Code, ev.Detail = lock.CodeOf(err), lock.Scrub(err.Error())
	}
	e.Store.Log(ev)
}

func (e *Env) setLedgerPending(id string, pending bool) {
	if e.Store != nil {
		_ = e.Store.SetLedgerPending(id, pending) // the next sync compares anyway
	}
}

// checkRemote refuses a lock that would live apart from the ledger. A ledger
// with no Dolt remote is the degenerate case: the lock would have no ledger
// beside it at all. A list that fails or does not parse is not compared, and
// the ledger reports its own failure later.
func (e *Env) checkRemote(ctx context.Context) error {
	url, err := e.Lock.RemoteURL(ctx)
	if err != nil {
		return err
	}
	return ledgerBeside(ctx, e.BD, e.Lock.RemoteName(), url).err()
}

// The fixes for a lock remote with no ledger beside it.
const (
	noDoltRemoteFix   = "add a Dolt remote on the same repository (`bd dolt remote add origin git+<that URL>`, e.g. git+https://… or git+ssh://…), or for a single-machine repo set lock.mode to none in .bops.json ({\"lock\": {\"mode\": \"none\"}})"
	otherDoltRepoFix  = "point it at the ledger's repository or set lock.remote"
	doltRemoteCheckID = "dolt_remote"
)

// besideResult is how the Dolt remote list compares with the lock remote.
// cause is empty when a Dolt remote matches; read is false when the list
// failed or did not parse, so nothing was compared.
type besideResult struct {
	read, matched bool
	cause, fix    string
}

func (b besideResult) err() error {
	if b.cause == "" {
		return nil
	}
	return &lock.Error{Code: lock.CodeRemoteMismatch, Err: errors.New(b.cause + "; " + b.fix)}
}

type doltRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// doltRemotes reads bd's Dolt remote list. ok is false when the list fails or
// does not parse.
func doltRemotes(ctx context.Context, bd bdx.Runner) (remotes []doltRemote, ok bool) {
	raw, err := bd.Run(ctx, "dolt", "remote", "list", "--json")
	if err != nil {
		return nil, false
	}
	if err := json.Unmarshal(raw, &remotes); err != nil {
		return nil, false
	}
	return remotes, true
}

// ledgerBeside compares the Dolt remote list with the lock remote at url.
func ledgerBeside(ctx context.Context, bd bdx.Runner, name, url string) besideResult {
	remotes, ok := doltRemotes(ctx, bd)
	if !ok {
		return besideResult{} // cannot compare, and the ledger has its own failure to report
	}
	return compareRemotes(remotes, name, url)
}

func compareRemotes(remotes []doltRemote, name, url string) besideResult {
	if len(remotes) == 0 {
		return besideResult{read: true, cause: fmt.Sprintf("lock remote %s (%s) has no ledger beside it: there is no Dolt remote, so the lock would not be shared with the ledger", name, url), fix: noDoltRemoteFix}
	}
	for _, r := range remotes {
		if lock.SameRemote(url, r.URL) {
			return besideResult{read: true, matched: true}
		}
	}
	return besideResult{read: true, cause: fmt.Sprintf("lock remote %s (%s) is not the Dolt remote %s, so the lock would not be shared with the ledger", name, url, remotes[0].URL), fix: otherDoltRepoFix}
}

// noLedgerRemote is the remote_mismatch error for lock.mode ref with no Dolt
// remote at all, or nil. Sync and sweep return it instead of bd's "no remote"
// pull failure; land and release, which have already done their work, turn
// it into a warning. A Dolt remote on another repository is checkRemote's and
// doctor's concern: the ledger still publishes there.
func (e *Env) noLedgerRemote(ctx context.Context) error {
	if e.LockOff || e.Lock == nil {
		return nil
	}
	remotes, ok := doltRemotes(ctx, e.BD)
	if !ok || len(remotes) > 0 {
		return nil
	}
	url, err := e.Lock.RemoteURL(ctx)
	if err != nil {
		return nil // the lock's own steps report a missing lock remote
	}
	return compareRemotes(remotes, e.Lock.RemoteName(), url).err()
}

// noLedgerRemoteWarning says a write was kept on this machine because the
// lock.mode ref ledger has no Dolt remote, or is "" when it has one.
func (e *Env) noLedgerRemoteWarning(ctx context.Context) string {
	if err := e.noLedgerRemote(ctx); err != nil {
		return noDoltRemoteWarning + ": " + lock.Scrub(err.Error())
	}
	return ""
}

// DoltRemoteCheck is doctor's dolt_remote check under lock.mode ref: the
// ledger's Dolt remote must be on the lock remote's repository. It fails with
// remote_mismatch when there is no Dolt remote or none matches, and is skipped
// when the lock remote is missing or the list cannot be read.
func DoltRemoteCheck(ctx context.Context, bd bdx.Runner, l *lock.Lock) lock.Check {
	url, err := l.RemoteURL(ctx)
	if err != nil {
		return lock.Check{ID: doltRemoteCheckID, Status: lock.CheckSkip, Detail: "not run: an earlier check failed"}
	}
	b := ledgerBeside(ctx, bd, l.RemoteName(), url)
	switch {
	case !b.read:
		return lock.Check{ID: doltRemoteCheckID, Status: lock.CheckSkip, Detail: "not run: `bd dolt remote list --json` failed or did not parse, so the Dolt remote was not compared"}
	case b.matched:
		return lock.Check{ID: doltRemoteCheckID, Status: lock.CheckOK, Detail: "a Dolt remote is on the same repository as " + l.RemoteName()}
	}
	return lock.Check{ID: doltRemoteCheckID, Status: lock.CheckFail, Code: lock.CodeRemoteMismatch, Detail: lock.Scrub(b.cause), Next: b.fix}
}

func (e *Env) show(ctx context.Context, id string) (*ledger.Issue, error) {
	raw, err := e.BD.Run(ctx, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	return ledger.DecodeIssue(raw)
}

// notClaimable returns why a plain claim must refuse this bead, or "". A set
// assignee refuses even with no ref, because a raw bd assignment has no ref.
func (e *Env) notClaimable(cur *ledger.Issue) string {
	if cur.Assignee != "" {
		return fmt.Sprintf("assigned to %q; if that claim is abandoned, use claim --force after it goes stale", cur.Assignee)
	}
	if runs := cur.LabelsWithPrefix("run:"); len(runs) > 0 {
		return fmt.Sprintf("carries run:%s; if that claim is abandoned, use claim --force after it goes stale", runs[0])
	}
	if cur.Status != "open" && cur.Status != "ready" {
		return "status is " + cur.Status
	}
	if l := blockingLabel(cur, e.BlockingLabels); l != "" {
		return "labelled " + l + ", which this repo's blocking_labels keep out of the pool; a person removes the label to release it"
	}
	return ""
}

// ledgerHolder says who the ledger shows holding cur when that is not this
// run, or a stranded run of this clone whose hold this run may finish. It is
// nil when the bead looks like this run's. With the lock off it is the only
// ownership check: a run: label decides, and the assignee only when there is
// no run: label, because bd's actor can be spelled differently from bops's.
func (e *Env) ledgerHolder(cur *ledger.Issue) *Holder {
	mine := map[string]bool{e.Run.ID: true}
	if e.Store != nil {
		for _, h := range e.Store.Holds() {
			if h.ID == cur.ID && h.Stranded {
				mine[h.Run] = true
			}
		}
	}
	runs := cur.LabelsWithPrefix("run:")
	for _, r := range runs {
		if !mine[r] {
			return &Holder{Actor: cur.Assignee, Run: r}
		}
	}
	if len(runs) == 0 && cur.Assignee != "" && cur.Assignee != e.Run.Actor {
		return &Holder{Actor: cur.Assignee}
	}
	return nil
}

// pushLedger publishes local Dolt commits. Every bead shares one Dolt ref, so
// ordinary work on other beads, on this machine or another, rejects a push
// until it pulls first. Every command that publishes the ledger goes through
// here, so none gives up on the first non-fast-forward (bops-bze).
func (e *Env) pushLedger(ctx context.Context) error {
	var err error
	for try := 0; try < maxPushTries; try++ {
		if try > 0 {
			e.sleepFor(time.Duration(try) * 500 * time.Millisecond)
			if _, pullErr := withLockRetry(ctx, e, "dolt", "pull"); pullErr != nil {
				err = pullErr
				continue
			}
		}
		if _, err = withLockRetry(ctx, e, "dolt", "push"); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return err
}

// noDoltRemoteWarning says a write was kept on this machine because the
// ledger has no Dolt remote to publish to.
const noDoltRemoteWarning = "no Dolt remote: ledger not published"

// unpublished reports a ledger with nowhere to publish: lock.mode none and bd
// lists no Dolt remote. Such a ledger is whole on this machine, so a command
// skips its pull and push and says so in warnings. bd 1.3.4 fails `dolt pull`
// with no remote ("Error 1105: no remote") and skips `dolt push` silently.
//
// It asks for the remote list rather than reading a pull's error. Under
// lock.mode ref it is always false, because there the ledger must be shared
// with the lock; and a list that fails or does not parse counts as a remote,
// so the pull and push run and fail as they always did.
func (e *Env) unpublished(ctx context.Context) bool {
	if !e.LockOff {
		return false
	}
	remotes, ok := doltRemotes(ctx, e.BD)
	return ok && len(remotes) == 0
}

// display makes the ledger show that actor's run holds the bead, writing only
// what differs, so a retry adds no duplicate comment and repeats no claim.
// With keepParked (sync's repair) a blocked or deferred status is left as
// is; a claim always shows the bead in progress.
func (e *Env) display(ctx context.Context, cur *ledger.Issue, actor, run string, comment, keepParked bool) error {
	id := cur.ID
	switch {
	case cur.Status == "closed":
		return nil
	case cur.Assignee == "" && (cur.Status == "open" || cur.Status == "ready"):
		if _, err := e.BD.Run(ctx, "update", id, "--claim"); err != nil {
			return err
		}
	case keepParked && (cur.Status == "blocked" || cur.Status == "deferred"):
		// A person parked a held bead after the claim; sync repairs who holds
		// it, never its status (bops-yr2).
		if cur.Assignee != actor {
			if _, err := e.BD.Run(ctx, "update", id, "--assignee", actor); err != nil {
				return err
			}
		}
	case cur.Assignee != actor || cur.Status != "in_progress":
		if _, err := e.BD.Run(ctx, "update", id, "--assignee", actor, "--status", "in_progress", "--force"); err != nil {
			return err
		}
	}
	for _, r := range cur.LabelsWithPrefix("run:") {
		if r != run {
			if _, err := e.BD.Run(ctx, "label", "remove", id, "run:"+r); err != nil {
				return err
			}
		}
	}
	if !cur.HasLabel("run:" + run) {
		if _, err := e.BD.Run(ctx, "label", "add", id, "run:"+run); err != nil {
			return err
		}
	}
	for _, l := range []string{"bops:hold", "bops:landing"} {
		if cur.HasLabel(l) {
			if _, err := e.BD.Run(ctx, "label", "remove", id, l); err != nil {
				return err
			}
		}
	}
	if !comment {
		return nil
	}
	return e.addClaimComment(ctx, id, actor, run, "")
}

func (e *Env) addClaimComment(ctx context.Context, id, actor, run, extra string) error {
	raw, err := e.BD.Run(ctx, "comments", id, "--json")
	if err != nil {
		return err
	}
	var comments []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &comments); err != nil {
		return err
	}
	prefix := "bops:claimed run=" + run + " "
	for _, c := range comments {
		if strings.HasPrefix(c.Text, prefix) {
			return nil
		}
	}
	_, err = e.BD.Run(ctx, "comments", "add", id, fmt.Sprintf("bops:claimed run=%s actor=%s machine=%s%s", run, actor, e.Run.Machine, extra))
	return err
}

// --------------------------------------------------------------- claim

// ClaimOut is the `claim` output.
type ClaimOut struct {
	ID            string    `json:"id"`
	Run           string    `json:"run"`
	Outcome       string    `json:"outcome"`
	ClaimedAt     time.Time `json:"claimed_at,omitzero"`
	ReadbackOK    bool      `json:"readback_ok,omitempty"`
	LedgerPending bool      `json:"ledger_pending,omitempty"`
	RefPending    bool      `json:"ref_pending,omitempty"`
	Holder        *Holder   `json:"holder,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	AgeMinutes    int       `json:"age_minutes,omitempty"`
	StaleMinutes  int       `json:"stale_minutes,omitempty"`
	Retry         string    `json:"retry,omitempty"`
	SideEffects   string    `json:"side_effects,omitempty"`
	// NextCandidate is, on lost or refused, the bead `next` would offer now.
	NextCandidate string `json:"next_candidate,omitempty"`
	// Skipped lists, for claim --next, the candidates lost or refused first.
	Skipped []ClaimSkip `json:"skipped,omitempty"`
	// Previous is, on a claim --force that took the bead, where the last
	// attempt stopped: its holder, last note, branch and merge request.
	Previous *Previous `json:"previous,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
	// handoff is what a takeover read of the bead, for Claim to fill
	// Previous from once the bead lock is let go.
	handoff *handoffSrc
}

// Negative reports a run that worked and an answer of no.
func (o *ClaimOut) Negative() bool { return o != nil && negative(o.Outcome) }

func (o *ClaimOut) fail(err error) (*ClaimOut, error) {
	o.Outcome = OutcomeError
	switch lock.CodeOf(err) {
	case lock.CodeUnreachable, lock.CodeAuth:
		o.Outcome, o.Retry, o.SideEffects = OutcomeUnreachable, retryAdvice, sideEffectsAdvice
	case lock.CodeContention:
		o.Retry = contentionAdvice
	}
	return o, err
}

// Claim takes a bead for this run. Creating the ref decides who owns it, and
// the ledger only catches up afterward:
//
//	bead lock --> already held? --> bd show --> create ref --> pull --> ledger writes --> push
//	    |             |               |            |            |            |
//	ref_contention   held         refused      lost, no     refused,     claimed,
//	                                           ledger       ref deleted  ledger_pending
//	                                           write
//
// Every path, --force included, starts by taking the bead's lock (bops-yph).
// A lost or refused claim names the bead `next` would offer instead.
func Claim(ctx context.Context, env *Env, id string, force bool) (*ClaimOut, error) {
	out, err := claimOne(ctx, env, id, force)
	if err == nil && out != nil && out.Negative() {
		out.NextCandidate = env.nextCandidate(ctx, map[string]bool{id: true}, &out.Warnings)
	}
	if out != nil && out.handoff != nil {
		// After the bead lock is let go, and bounded by handoffClock within
		// the command's own deadline: the reads are advisory and must not
		// hold the bead or the claim's answer long.
		hctx, cancel := context.WithTimeout(ctx, handoffClock)
		env.fillPrevious(hctx, out.handoff.cur, out.handoff.prev, &out.Warnings)
		cancel()
		out.Previous, out.handoff = out.handoff.prev, nil
	}
	return out, err
}

func claimOne(ctx context.Context, env *Env, id string, force bool) (*ClaimOut, error) {
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	out := &ClaimOut{ID: id, Run: env.Run.ID, Warnings: []string{}}
	name := "claim"
	if force {
		name = "claim --force"
	}
	unlock, err := env.holdBead(ctx, name, id, &out.Warnings)
	if err != nil {
		return out.fail(err)
	}
	defer unlock()
	if _, err := env.finishRelease(ctx, id); err != nil {
		return out.fail(err)
	}
	if force {
		forced, err := claimForce(ctx, env, id)
		if forced == nil {
			return out.fail(err)
		}
		forced.Warnings = append(out.Warnings, forced.Warnings...)
		return forced, err
	}
	if env.LockOff {
		return claimLedger(ctx, env, id, out)
	}
	if err := env.checkRemote(ctx); err != nil {
		return out.fail(err)
	}
	if done, err := env.alreadyHeld(ctx, id, out); done || err != nil {
		return out, err
	}
	cur, err := env.show(ctx, id)
	if err != nil {
		return nil, err
	}
	if why := env.notClaimable(cur); why != "" {
		return env.refuseOrLose(ctx, id, why, out), nil
	}
	res, err := env.Lock.Acquire(ctx, id)
	if err != nil {
		env.event("claim", id, "ref_create", OutcomeUnreachable, "", err)
		return out.fail(err)
	}
	out.Warnings = append(out.Warnings, res.Warnings...)
	if res.Outcome == lock.Lost {
		if res.Holder != nil && res.Holder.Run == env.Run.ID {
			out.Outcome = OutcomeHeld // two commands in one worktree raced
			env.event("claim", id, "ref_create", OutcomeHeld, res.Holder.SHA, nil)
			return out, nil
		}
		out.Outcome, out.Holder = OutcomeLost, env.holder(res.Holder)
		env.event("claim", id, "ref_create", OutcomeLost, "", nil)
		return out, nil
	}
	env.event("claim", id, "ref_create", OutcomeClaimed, res.SHA, nil)

	if _, pullErr := withLockRetry(ctx, env, "dolt", "pull"); pullErr != nil {
		out.Warnings = append(out.Warnings, "ledger not pulled: "+pullErr.Error())
	} else if fresh, showErr := env.show(ctx, id); showErr == nil {
		if why := env.notClaimable(fresh); why != "" {
			return env.giveBack(ctx, id, res.SHA, why, out)
		}
		cur = fresh
	}

	out.Outcome, out.ClaimedAt = OutcomeClaimed, env.now()
	if err := env.publishClaim(ctx, cur, "", out); err != nil {
		out.LedgerPending = true
		out.Warnings = append(out.Warnings, "ledger display pending: "+err.Error())
		env.setLedgerPending(id, true)
		env.event("claim", id, "ledger", "pending", res.SHA, err)
		return out, nil
	}
	env.event("claim", id, "ledger", "written", res.SHA, nil)
	return out, nil
}

// claimLedger is a plain claim with the lock off (lock.mode none). bd's own
// compare-and-set on `update --claim` decides. That holds on one replica, so
// it separates the worktrees of one machine and nothing more:
//
//	already held? --> pull (best effort) --> bd show --> bd update --claim --> journal --> labels, comment, push
//	    |                                       |               |
//	    held                                 refused          lost (someone else's assignee)
//
// With no Dolt remote there is nothing to pull or push: both are skipped
// with the warning every other command gives (SPEC §3.0).
func claimLedger(ctx context.Context, env *Env, id string, out *ClaimOut) (*ClaimOut, error) {
	out.Warnings = append(out.Warnings, lock.OffWarning)
	if done, err := env.alreadyHeld(ctx, id, out); done || err != nil {
		return out, err
	}
	publish := env.pullUnlessUnpublished(ctx, out)
	cur, err := env.show(ctx, id)
	if err != nil {
		return nil, err
	}
	if why := env.notClaimable(cur); why != "" {
		out.Outcome, out.Reason = OutcomeRefused, why
		env.event("claim", id, "precondition", OutcomeRefused, "", nil)
		return out, nil
	}
	if _, err := env.BD.Run(ctx, "update", id, "--claim"); err != nil {
		if fresh, showErr := env.show(ctx, id); showErr == nil && fresh.Assignee != "" {
			out.Outcome, out.Holder = OutcomeLost, &Holder{Actor: fresh.Assignee}
			if h := env.ledgerHolder(fresh); h != nil {
				out.Holder = h
			}
			env.event("claim", id, "ledger_claim", OutcomeLost, "", nil)
			return out, nil
		}
		env.event("claim", id, "ledger_claim", OutcomeError, "", err)
		return out.fail(err)
	}
	return env.takeLedger(ctx, id, "", publish, out)
}

// pullUnlessUnpublished is a lock-off claim's best-effort pull. It reports
// whether the ledger has a Dolt remote to publish to; when it has none, the
// pull is skipped and the claim says the ledger was not published.
func (e *Env) pullUnlessUnpublished(ctx context.Context, out *ClaimOut) bool {
	if e.unpublished(ctx) {
		out.Warnings = append(out.Warnings, noDoltRemoteWarning)
		return false
	}
	if _, err := withLockRetry(ctx, e, "dolt", "pull"); err != nil {
		out.Warnings = append(out.Warnings, "ledger not pulled: "+err.Error())
	}
	return true
}

// takeLedger journals a hold the ledger has just given this run, then writes
// the rest of the display. With the lock off nothing repairs the display
// later, so a failure after the claim is a warning, not ledger_pending.
// publish false (no Dolt remote) skips the push.
func (e *Env) takeLedger(ctx context.Context, id, extra string, publish bool, out *ClaimOut) (*ClaimOut, error) {
	if err := e.Lock.Journal.Confirm(id, ""); err != nil { // no sha: a hold with no ref
		if _, undoErr := e.BD.Run(ctx, "update", id, "--assignee", "", "--status", "open"); undoErr != nil {
			out.Warnings = append(out.Warnings, "the ledger still shows this claim; clear it with `bd update "+id+" --assignee '' --status open`: "+undoErr.Error())
		}
		return out.fail(&lock.Error{Code: lock.CodeHoldNotRecorded, Err: err})
	}
	out.Outcome, out.ClaimedAt = OutcomeClaimed, e.now()
	e.event("claim", id, "ledger_claim", OutcomeClaimed, "", nil)
	cur, err := e.show(ctx, id)
	if err == nil {
		err = e.display(ctx, cur, e.Run.Actor, e.Run.ID, false, false)
	}
	if err == nil {
		err = e.addClaimComment(ctx, id, e.Run.Actor, e.Run.ID, extra)
	}
	if err == nil {
		if got, showErr := e.show(ctx, id); showErr == nil {
			out.ReadbackOK = got.Assignee != "" && got.HasLabel("run:"+e.Run.ID)
		}
	}
	if err != nil {
		out.Warnings = append(out.Warnings, "ledger display incomplete: "+err.Error())
	}
	if !publish {
		return out, nil
	}
	if err := e.pushLedger(ctx); err != nil {
		out.Warnings = append(out.Warnings, "ledger not pushed: "+err.Error())
	}
	return out, nil
}

// refuseOrLose explains a bead the ledger says is taken. Worktrees of one clone
// share a ledger, so a racing command can see the winner's assignee before the
// ref would have told it. If a live claim exists, the ref gives the real answer.
func (e *Env) refuseOrLose(ctx context.Context, id, why string, out *ClaimOut) *ClaimOut {
	if ref, err := e.Lock.Read(ctx, id); err == nil && ref != nil && ref.State == lock.StateHeld {
		if ref.Run == e.Run.ID {
			out.Outcome = OutcomeHeld
		} else {
			out.Outcome, out.Holder = OutcomeLost, e.holder(ref)
		}
		e.event("claim", id, "precondition", out.Outcome, ref.SHA, nil)
		return out
	}
	out.Outcome, out.Reason = OutcomeRefused, why
	e.event("claim", id, "precondition", OutcomeRefused, "", nil)
	return out
}

// alreadyHeld handles a repeat claim, including one whose first attempt never
// got its reply.
func (e *Env) alreadyHeld(ctx context.Context, id string, out *ClaimOut) (bool, error) {
	entry, ok := e.Run.Held[id]
	if !ok {
		return false, nil
	}
	if e.LockOff { // no ref to settle against: the journal is the answer
		out.Outcome, out.LedgerPending = OutcomeHeld, entry.LedgerPending
		return true, nil
	}
	live, _, err := e.Lock.Live(ctx, id)
	if err != nil {
		_, err = out.fail(err)
		return true, err
	}
	state, err := e.Lock.Settle(id, live)
	if err != nil {
		_, err = out.fail(err)
		return true, err
	}
	switch state {
	case lock.Discarded:
		return false, nil
	case lock.Displaced:
		if err := e.Lock.Journal.Drop(id); err != nil {
			_, err = out.fail(err)
			return true, err
		}
		return false, nil
	case lock.Adopted:
		if entry.Phase == runstate.PhasePushing { // its ledger writes never ran
			entry.LedgerPending = true
			e.setLedgerPending(id, true)
		}
	}
	out.Outcome, out.LedgerPending = OutcomeHeld, entry.LedgerPending
	return true, nil
}

// giveBack undoes a claim on a bead the pull showed was taken or changed.
func (e *Env) giveBack(ctx context.Context, id, sha, why string, out *ClaimOut) (*ClaimOut, error) {
	out.Outcome, out.Reason = OutcomeRefused, why
	if _, err := e.Lock.Delete(ctx, id, sha); err != nil {
		out.RefPending = true
		out.Warnings = append(out.Warnings, "ref not removed yet, sync will retry: "+err.Error())
	}
	e.event("claim", id, "pull_check", OutcomeRefused, sha, nil)
	return out, nil
}

func (e *Env) publishClaim(ctx context.Context, cur *ledger.Issue, extra string, out *ClaimOut) error {
	if err := e.display(ctx, cur, e.Run.Actor, e.Run.ID, false, false); err != nil {
		return err
	}
	if err := e.addClaimComment(ctx, cur.ID, e.Run.Actor, e.Run.ID, extra); err != nil {
		return err
	}
	if got, err := e.show(ctx, cur.ID); err == nil {
		out.ReadbackOK = got.Assignee != "" && got.HasLabel("run:"+e.Run.ID)
	}
	if !out.ReadbackOK {
		return errors.New("readback did not show the assignee and run label")
	}
	return e.pushLedger(ctx)
}

// checkHold settles any unfinished ref write and reports whether this run
// still owns the bead. A legacy hold has no ref to compare.
func (e *Env) checkHold(ctx context.Context, id string) (runstate.Entry, bool, error) {
	entry, ok := e.Lock.Journal.Entry(id)
	if !ok {
		return entry, false, nil
	}
	if entry.Legacy() {
		return entry, true, nil
	}
	live, _, err := e.Lock.Live(ctx, id)
	if err != nil {
		return entry, false, err
	}
	state, err := e.Lock.Settle(id, live)
	if err != nil {
		return entry, false, err
	}
	if state == lock.Displaced || state == lock.Discarded {
		return entry, false, nil
	}
	entry, ok = e.Lock.Journal.Entry(id)
	return entry, ok, nil
}

// dropLost forgets a hold that is gone and says who has the bead now.
func (e *Env) dropLost(ctx context.Context, id string) (*Holder, error) {
	if err := e.Lock.Journal.Drop(id); err != nil {
		return nil, err
	}
	ref, err := e.Lock.Read(ctx, id)
	if err != nil {
		return nil, nil // the hold is gone either way, and the holder is a courtesy
	}
	return e.holder(ref), nil
}

func (e *Env) refuseStale(out *ClaimOut, why string, age time.Duration) (*ClaimOut, error) {
	out.Outcome, out.Reason = OutcomeRefused, why
	out.AgeMinutes, out.StaleMinutes = int(age.Minutes()), int(e.staleAfter().Minutes())
	e.event("claim", out.ID, "force_check", OutcomeRefused, "", nil)
	return out, nil
}

// claimForce is the single recovery path for an expired claim or an orphan. It
// replaces the ref with a lease on the value it just read, so of two takeovers
// exactly one wins. A landing bead is refused: its work is done.
func claimForce(ctx context.Context, env *Env, id string) (*ClaimOut, error) {
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	out := &ClaimOut{ID: id, Run: env.Run.ID, Warnings: []string{}}
	if env.LockOff {
		out.Warnings = append(out.Warnings, lock.OffWarning)
	} else if err := env.checkRemote(ctx); err != nil {
		return out.fail(err)
	}
	if done, err := env.alreadyHeld(ctx, id, out); done || err != nil {
		return out, err
	}
	var ref *lock.Ref // with the lock off there is never a ref: the ledger branch below decides
	publish := true
	if env.LockOff {
		publish = env.pullUnlessUnpublished(ctx, out)
	} else if _, err := withLockRetry(ctx, env, "dolt", "pull"); err != nil {
		return out.fail(&lock.Error{Code: lock.CodeUnreachable, Err: err})
	}
	if !env.LockOff {
		var err error
		if ref, err = env.Lock.Read(ctx, id); err != nil {
			return out.fail(err)
		}
	}
	cur, err := env.show(ctx, id)
	if err != nil {
		return nil, err
	}
	now, window := env.now(), env.staleAfter()
	expected, previous := "", ""
	prev := &Previous{}
	if ref != nil {
		expected, previous = ref.SHA, ref.Run
		age := now.Sub(ref.At)
		prev.Actor, prev.Run, prev.AgeMinutes = ref.Actor, ref.Run, max(0, int(age.Minutes()))
		switch {
		case ref.State == lock.StateLanding:
			return env.refuseStale(out, "its work has landed and is waiting on the PR; use claim --resume as the holder, or see the recovery section of PROCESS", age)
		case ref.Anomalous(now):
			return env.refuseStale(out, fmt.Sprintf("the claim's time %s is ahead of this machine's clock; check both clocks", ref.At.Format(time.RFC3339)), 0)
		case !ref.Stale(now, window):
			return env.refuseStale(out, fmt.Sprintf("held by %s and not stale; a person can wait or ask the holder to release it", ref.Actor), age)
		}
	} else {
		runs := cur.LabelsWithPrefix("run:")
		if cur.Assignee == "" && len(runs) == 0 {
			out.Outcome, out.Reason = OutcomeRefused, "nothing to take over; use a plain claim"
			return out, nil
		}
		if len(runs) > 0 {
			previous = runs[0]
		}
		age := now.Sub(cur.UpdatedAt)
		prev.Actor, prev.Run, prev.AgeMinutes = cur.Assignee, previous, max(0, int(age.Minutes()))
		if cur.UpdatedAt.IsZero() || cur.UpdatedAt.After(now) || age <= window {
			return env.refuseStale(out, fmt.Sprintf("assigned to %q with recent activity; a person can wait or ask them to release it", cur.Assignee), age)
		}
	}
	if env.LockOff {
		// The forced write is the takeover. Unlike a lease, it does not stop
		// a second takeover racing this one.
		if _, err := env.BD.Run(ctx, "update", id, "--assignee", env.Run.Actor, "--status", "in_progress", "--force"); err != nil {
			env.event("claim", id, "ledger_takeover", OutcomeError, "", err)
			return out.fail(err)
		}
		out, err := env.takeLedger(ctx, id, " takeover="+previous, publish, out)
		if err == nil && out.Outcome == OutcomeClaimed {
			out.handoff = &handoffSrc{cur, prev}
		}
		return out, err
	}
	res, err := env.Lock.Replace(ctx, id, expected, lock.StateHeld)
	if err != nil {
		env.event("claim", id, "ref_replace", OutcomeUnreachable, "", err)
		return out.fail(err)
	}
	out.Warnings = append(out.Warnings, res.Warnings...)
	if res.Outcome == lock.Lost {
		out.Outcome, out.Holder = OutcomeLost, env.holder(res.Holder)
		env.event("claim", id, "ref_replace", OutcomeLost, "", nil)
		return out, nil
	}
	env.event("claim", id, "ref_replace", OutcomeClaimed, res.SHA, nil)
	out.Outcome, out.ClaimedAt = OutcomeClaimed, now
	env.finishTakeover(ctx, cur, res.SHA, " takeover="+previous, out)
	out.handoff = &handoffSrc{cur, prev}
	return out, nil
}

// ClaimResume takes back a landing bead for PR rework (D25), or the caller's
// own held bead from another machine (bops-xwr; resumeRefusal has the rule).
// The ref's state, actor and machine decide, not the ledger's labels, and the
// lease separates two machines sharing one actor name.
func ClaimResume(ctx context.Context, env *Env, id string) (*ClaimOut, error) {
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	out := &ClaimOut{ID: id, Run: env.Run.ID, Warnings: []string{}}
	unlock, err := env.holdBead(ctx, "claim --resume", id, &out.Warnings)
	if err != nil {
		return out.fail(err)
	}
	defer unlock()
	if _, err := env.finishRelease(ctx, id); err != nil {
		return out.fail(err)
	}
	if env.LockOff {
		// SPEC decides a resume by the ref's state and actor; with no ref
		// that rule is not defined yet, so this refuses rather than guess.
		out.Outcome, out.Reason = OutcomeRefused, "claim --resume needs the claim ref, and lock.mode is none; to rework a landing bead, run `bd label remove "+id+" bops:landing` and `bd update "+id+" --status open`, then claim it"
		out.Warnings = append(out.Warnings, lock.OffWarning)
		env.event("claim", id, "resume_check", OutcomeRefused, "", nil)
		return out, nil
	}
	if err := env.checkRemote(ctx); err != nil {
		return out.fail(err)
	}
	if done, err := env.alreadyHeld(ctx, id, out); done || err != nil {
		return out, err
	}
	ref, err := env.Lock.Read(ctx, id)
	if err != nil {
		return out.fail(err)
	}
	refuse := func(why string) (*ClaimOut, error) {
		out.Outcome, out.Reason = OutcomeRefused, why
		env.event("claim", id, "resume_check", OutcomeRefused, "", nil)
		return out, nil
	}
	if ref == nil {
		return refuse("no claim exists for this bead, so there is nothing to resume; a merged bead is closed by sweep")
	}
	if why := resumeRefusal(ref, env.Run.Actor, env.Run.Machine); why != "" {
		out.Holder = env.holder(ref)
		return refuse(why)
	}
	res, err := env.Lock.Replace(ctx, id, ref.SHA, lock.StateHeld)
	if err != nil {
		env.event("claim", id, "ref_replace", OutcomeUnreachable, "", err)
		return out.fail(err)
	}
	if res.Outcome == lock.Lost {
		out.Outcome, out.Holder = OutcomeLost, env.holder(res.Holder)
		env.event("claim", id, "ref_replace", OutcomeLost, "", nil)
		return out, nil
	}
	env.event("claim", id, "ref_replace", OutcomeClaimed, res.SHA, nil)
	out.Outcome, out.ClaimedAt = OutcomeClaimed, env.now()
	if _, err := withLockRetry(ctx, env, "dolt", "pull"); err != nil {
		out.Warnings = append(out.Warnings, "ledger not pulled: "+err.Error())
	}
	cur, err := env.show(ctx, id)
	if err != nil {
		out.LedgerPending = true
		out.Warnings = append(out.Warnings, "ledger display pending: "+err.Error())
		env.setLedgerPending(id, true)
		return out, nil
	}
	env.finishTakeover(ctx, cur, res.SHA, " resumed="+ref.Run, out)
	return out, nil
}

// resumeRefusal says why claim --resume may not take ref, or "" if it may.
// Only the ref's actor resumes. A landing bead resumes from any machine, for
// PR rework. A held bead resumes only from another machine than the one that
// holds it, so a person can carry their own work from laptop to desktop
// (bops-xwr): worktrees of one machine share the actor, and must not take
// beads from each other, and a dead worktree's hold is a stranded hold. A held
// ref with no machine key was written by a bops before bops-xwr and is refused
// rather than guessed at.
func resumeRefusal(ref *lock.Ref, actor, machine string) string {
	switch {
	case ref.State != lock.StateLanding && ref.State != lock.StateHeld:
		return "the claim ref's state is unreadable; read it (PROCESS §12) before acting"
	case ref.Actor != actor:
		if ref.State == lock.StateHeld {
			return fmt.Sprintf("held by %s; only that actor can resume it, from another machine", ref.Actor)
		}
		return fmt.Sprintf("only %s can resume this bead", ref.Actor)
	case ref.State == lock.StateLanding:
		return ""
	case ref.Machine == "":
		return "held by run " + ref.Run + " on a machine its claim ref does not name (written by an older bops); release it on that machine, or wait and use claim --force"
	case ref.Machine == machine:
		return "held by run " + ref.Run + " on this machine; resume moves a bead between machines. Work in that worktree, or release it from there (a removed worktree's hold is listed under stranded_holds by bops sync)"
	}
	return ""
}

// finishTakeover updates the display after the ref has changed hands.
func (e *Env) finishTakeover(ctx context.Context, cur *ledger.Issue, sha, extra string, out *ClaimOut) {
	if err := e.publishClaim(ctx, cur, extra, out); err != nil {
		out.LedgerPending = true
		out.Warnings = append(out.Warnings, "ledger display pending: "+err.Error())
		e.setLedgerPending(cur.ID, true)
		e.event("claim", cur.ID, "ledger", "pending", sha, err)
		return
	}
	e.event("claim", cur.ID, "ledger", "written", sha, nil)
}

// ------------------------------------------------------------- release

// ReleaseReasons are the allowed `--reason` values.
var ReleaseReasons = []string{"done", "blocked", "scope", "abandoned", "human"}

// ReleaseOut is the `release` output.
type ReleaseOut struct {
	ID            string   `json:"id"`
	Reason        string   `json:"reason"`
	Outcome       string   `json:"outcome"`
	LedgerPending bool     `json:"ledger_pending,omitempty"`
	RefPending    bool     `json:"ref_pending,omitempty"`
	Holder        *Holder  `json:"holder,omitempty"`
	Detail        string   `json:"detail,omitempty"`
	SavedTo       string   `json:"saved_to,omitempty"` // where the text of a release that did not run went
	Warnings      []string `json:"warnings,omitempty"`
	OpID          string   `json:"op_id,omitempty"`
	Replayed      bool     `json:"replayed,omitempty"` // op_id was already recorded; nothing written
}

// Negative reports a run that worked and an answer of no.
func (o *ReleaseOut) Negative() bool { return o != nil && negative(o.Outcome) }

func (o *ReleaseOut) fail(err error) (*ReleaseOut, error) {
	o.Outcome = OutcomeError
	if c := lock.CodeOf(err); c == lock.CodeUnreachable || c == lock.CodeAuth {
		o.Outcome = OutcomeUnreachable
	}
	return o, err
}

// holdFor finds a hold this run may act on: its own, or one stranded by a
// removed worktree of this clone. A live sibling worktree's hold is not.
func (e *Env) holdFor(id string) (runstate.Entry, bool) {
	if h, ok := e.Run.Held[id]; ok {
		return h, true
	}
	if e.Store == nil {
		return runstate.Entry{}, false
	}
	for _, h := range e.Store.Holds() {
		if h.ID == id && h.Stranded {
			return h.Entry, true
		}
	}
	return runstate.Entry{}, false
}

// Release gives a claim back with a classified reason. Only the holder can.
// The ref is checked first, so a displaced holder writes nothing to the ledger.
func Release(ctx context.Context, env *Env, id, reason string) (*ReleaseOut, error) {
	return ReleaseWithText(ctx, env, id, reason, "")
}

// ReleaseWithText is Release with optional free text. A non-empty text goes
// into the release's one bops:released comment, below the event line, through
// a private file; it is written only when the release is, so a lost or refused
// release writes none of it.
func ReleaseWithText(ctx context.Context, env *Env, id, reason, text string) (*ReleaseOut, error) {
	return ReleaseOp(ctx, env, id, reason, text, "")
}

// ReleaseOp is ReleaseWithText with an operation id (see opid.go); "" derives
// one from the run, reason and text. When this run no longer holds the bead
// and its bops:released comment carries the op id, that release landed: it
// writes nothing and answers released with replayed set. While the hold
// remains, the release runs in full, since a release that stopped part way
// must finish (sync would otherwise keep a claim given back on the ledger).
func ReleaseOp(ctx context.Context, env *Env, id, reason, text, op string) (*ReleaseOut, error) {
	out, err := releaseWithText(ctx, env, id, reason, text, op)
	// Text from stdin must outlive a release that did not run, or whose
	// ledger write did not (sync settles the claim but never the comment).
	if out != nil && text != "" && (out.Outcome != OutcomeReleased || out.LedgerPending) {
		out.SavedTo = saveText("release", text, &out.Warnings)
	}
	return out, err
}

func releaseWithText(ctx context.Context, env *Env, id, reason, text, op string) (*ReleaseOut, error) {
	if !contains(ReleaseReasons, reason) {
		return nil, fmt.Errorf("release: reason must be one of %s", strings.Join(ReleaseReasons, "|"))
	}
	if text != "" && strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: release text is empty", ErrPrecondition)
	}
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	op, err := opID(op, "release", id, env.Run.ID, reason, text)
	if err != nil {
		return nil, err
	}
	out := &ReleaseOut{ID: id, Reason: reason, Warnings: []string{}, OpID: op}
	unlock, err := env.holdBead(ctx, "release", id, &out.Warnings)
	if err != nil {
		return out.fail(err)
	}
	defer unlock()
	finished, err := env.finishRelease(ctx, id)
	if err != nil {
		return out.fail(err)
	}
	entry, ok := env.holdFor(id)
	if finished || !ok {
		if env.replayRelease(ctx, id, op, out) {
			return out, nil
		}
	}
	if finished {
		out.Outcome, out.Detail = OutcomeRefused, "an earlier release of "+id+" had not finished deleting its claim; that is done now, and this run no longer holds it"
		return out, nil
	}
	if !ok {
		out.Outcome, out.Detail = OutcomeRefused, "this run does not hold "+id+"; claim --force is how a bead changes hands"
		return out, nil
	}
	sha := entry.SHA
	publish := true
	if env.LockOff {
		out.Warnings = append(out.Warnings, lock.OffWarning)
		cur, err := env.show(ctx, id)
		if err != nil {
			return out.fail(err)
		}
		if holder := env.ledgerHolder(cur); holder != nil {
			if err := env.Lock.Journal.Drop(id); err != nil {
				return out.fail(err)
			}
			out.Outcome, out.Holder = OutcomeLost, holder
			env.event("release", id, "ledger_check", OutcomeLost, "", nil)
			return out, nil
		}
		if refused, err := env.refuseUnlanded(cur, id, reason, sha, out); refused || err != nil {
			return out, err
		}
		if env.unpublished(ctx) {
			publish = false
			out.Warnings = append(out.Warnings, noDoltRemoteWarning)
		}
	} else {
		if entry.Legacy() && reason == "done" { // no ref to verify; check the ledger alone
			cur, err := env.show(ctx, id)
			if err != nil {
				return out.fail(err)
			}
			if refused, err := env.refuseUnlanded(cur, id, reason, sha, out); refused || err != nil {
				return out, err
			}
		}
		if !entry.Legacy() {
			var done bool
			var err error
			if sha, done, err = env.verifyHold(ctx, id, reason, out); done || err != nil {
				return out, err
			}
		}
		// The bead is still given back; only its ledger stays on this machine.
		if w := env.noLedgerRemoteWarning(ctx); w != "" {
			publish = false
			out.Warnings = append(out.Warnings, w)
		}
	}
	if err := env.releaseDisplay(ctx, id, reason, text, op, publish); err != nil {
		out.LedgerPending = true
		out.Warnings = append(out.Warnings, "ledger display pending: "+err.Error())
		env.event("release", id, "ledger", "pending", sha, err)
	}
	out.Outcome = OutcomeReleased
	if entry.Legacy() || reason == "done" || env.LockOff {
		if err := env.Lock.Journal.Drop(id); err != nil {
			return out.fail(err)
		}
		env.event("release", id, "done", OutcomeReleased, sha, nil)
		return out, nil
	}
	res, err := env.Lock.Delete(ctx, id, sha)
	if err != nil {
		out.RefPending = true
		out.Warnings = append(out.Warnings, "ref not removed yet, sync will retry: "+err.Error())
		env.event("release", id, "ref_delete", "pending", sha, err)
		return out, nil
	}
	if res.Outcome == lock.Lost {
		out.Outcome, out.Holder = OutcomeLost, env.holder(res.Holder)
	}
	env.event("release", id, "ref_delete", out.Outcome, sha, nil)
	return out, nil
}

// replayRelease reports whether id carries the bops:released comment of op,
// and if so fills out as that release's answer. A failed read is a warning;
// the caller then answers as it would have without op ids.
func (e *Env) replayRelease(ctx context.Context, id, op string, out *ReleaseOut) bool {
	rec, err := e.findOp(ctx, id, evReleased, op)
	if err != nil {
		out.Warnings = append(out.Warnings, "earlier releases not checked for op "+op+": "+lock.Scrub(err.Error()))
		return false
	}
	if rec == nil {
		return false
	}
	if rec.fields["reason"] != out.Reason {
		// The same op id with another reason is not this release.
		out.Warnings = append(out.Warnings, "op "+op+" was recorded with reason "+rec.fields["reason"]+", not "+out.Reason)
		return false
	}
	out.Outcome, out.Replayed, out.Detail = OutcomeReleased, true, replayNote(op)
	e.event("release", id, "op", "replayed", "", nil)
	// The first run's ledger push may have failed: publish again.
	e.republish(ctx, &out.Warnings)
	return true
}

// refuseUnlanded refuses a done release of a bead with no landed record. Such
// a landing has no PR for sweep to check, stale treats landing as waiting and
// next excludes it, so nothing would report it again. It runs before the ref
// or the ledger changes, and keeps the hold so the run can land and retry.
func (e *Env) refuseUnlanded(cur *ledger.Issue, id, reason, sha string, out *ReleaseOut) (bool, error) {
	if reason != "done" {
		return false, nil
	}
	landed, err := cur.Landed()
	if err != nil {
		_, err = out.fail(err)
		return true, err
	}
	if len(landed) > 0 {
		return false, nil
	}
	out.Outcome = OutcomeRefused
	out.Detail = id + " has no land record: run `bops land " + id + " --ref <pr-url> --sha <sha>` first, or give it back with --reason scope"
	e.event("release", id, "land_check", OutcomeRefused, sha, nil)
	return true, nil
}

// verifyHold confirms the ref is still this run's, and for a done release
// marks it landing before any ledger write. It returns the ref's sha.
func (e *Env) verifyHold(ctx context.Context, id, reason string, out *ReleaseOut) (string, bool, error) {
	ref, err := e.Lock.Read(ctx, id)
	if err != nil {
		_, err = out.fail(err)
		return "", true, err
	}
	live := ""
	if ref != nil {
		live = ref.SHA
	}
	state, err := e.Lock.Settle(id, live)
	if err != nil {
		_, err = out.fail(err)
		return "", true, err
	}
	if state == lock.Displaced || state == lock.Discarded {
		if err := e.Lock.Journal.Drop(id); err != nil {
			_, err = out.fail(err)
			return "", true, err
		}
		out.Outcome, out.Holder = OutcomeLost, e.holder(ref)
		e.event("release", id, "ref_check", OutcomeLost, live, nil)
		return "", true, nil
	}
	if reason != "done" {
		return live, false, nil
	}
	// Still this run's: refuse a done with nothing landed before the ref
	// becomes landing, so a displaced holder hears lost, not this.
	cur, err := e.show(ctx, id)
	if err != nil {
		_, err = out.fail(err)
		return "", true, err
	}
	if refused, err := e.refuseUnlanded(cur, id, reason, live, out); refused || err != nil {
		return "", true, err
	}
	res, err := e.Lock.Replace(ctx, id, live, lock.StateLanding)
	if err != nil {
		_, err = out.fail(err)
		return "", true, err
	}
	if res.Outcome == lock.Lost {
		if err := e.Lock.Journal.Drop(id); err != nil {
			_, err = out.fail(err)
			return "", true, err
		}
		out.Outcome, out.Holder = OutcomeLost, e.holder(res.Holder)
		return "", true, nil
	}
	return res.SHA, false, nil
}

// releaseDisplay writes what `release` shows on the ledger. A done bead stays
// in_progress until sweep closes it; every other reason returns it to a status
// bd ready understands. A non-empty text follows the event line in the same
// comment. It pushes only when publish is set.
func (e *Env) releaseDisplay(ctx context.Context, id, reason, text, op string, publish bool) error {
	cur, err := e.show(ctx, id)
	if err != nil {
		return err
	}
	update := []string{"update", id, "--assignee", ""}
	switch reason {
	case "blocked":
		update = append(update, "--status", "blocked")
	case "human", "scope", "abandoned":
		update = append(update, "--status", "open")
	}
	if _, err := e.BD.Run(ctx, update...); err != nil {
		return err
	}
	for _, r := range cur.LabelsWithPrefix("run:") {
		if _, err := e.BD.Run(ctx, "label", "remove", id, "run:"+r); err != nil {
			return err
		}
	}
	label := map[string]string{"human": LabelNeedsHuman, "scope": "bops:hold", "abandoned": "bops:hold", "done": "bops:landing"}[reason]
	if label != "" {
		if _, err := e.BD.Run(ctx, "label", "add", id, label); err != nil {
			return err
		}
	}
	// Always written, even when an earlier comment carries op: that may be
	// an earlier hold's release by this run, whose record must not stand in
	// for this one. A release that stopped part way may so write it twice.
	ev := fmt.Sprintf("%s run=%s reason=%s op=%s", evReleased, e.Run.ID, reason, op)
	if err := e.addReleasedComment(ctx, id, ev, text); err != nil {
		return err
	}
	if !publish {
		return nil
	}
	return e.pushLedger(ctx)
}

// addReleasedComment writes the release event. The fixed-vocabulary line
// alone may be an argument (SPEC §1.7); with text it goes through a file.
func (e *Env) addReleasedComment(ctx context.Context, id, ev, text string) error {
	if text == "" {
		_, err := e.BD.Run(ctx, "comments", "add", id, ev)
		return err
	}
	path, cleanup, err := writeTemp(ev + "\n" + text)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = e.BD.Run(ctx, "comments", "add", id, "-f", path)
	return err
}

// ---------------------------------------------------------------- sync

// SyncOut is the `sync` output.
type SyncOut struct {
	Pulled      bool   `json:"pulled"`
	Pushed      bool   `json:"pushed"`
	Revision    string `json:"revision"`
	Unpublished int    `json:"unpublished"`
	// StrandedHolds are beads held by a worktree that no longer exists. They
	// are released from any worktree of this clone, or wait for claim --force.
	StrandedHolds []string `json:"stranded_holds"`
	// LostClaims are beads this clone thought it held and no longer does.
	LostClaims []string `json:"lost_claims"`
	// Deferred are holds this sync left unchecked: another bops process on
	// this machine was working on them, or they changed while it ran. The
	// next sync checks them (bops-yph).
	Deferred []string `json:"deferred"`
	Warnings []string `json:"warnings,omitempty"`
}

type cloneHold struct {
	id       string
	run      string
	stranded bool
	entry    runstate.Entry
}

// clearOwnDisplay returns a bead to the pool when this clone's claim ref is
// gone and nobody took it: otherwise the ledger keeps showing a claim nobody
// holds, which a plain claim refuses and --force waits stale_minutes on. It
// acts only on a bead still in progress under this run's label. Another
// run's claim repairs the display itself; a landing bead with no ref is
// sweep's crash-recovery case; a closed bead is done.
func (e *Env) clearOwnDisplay(ctx context.Context, h cloneHold) error {
	cur, err := e.show(ctx, h.id)
	if err != nil {
		return err
	}
	if h.run == "" || cur.Status != "in_progress" || !cur.HasLabel("run:"+h.run) || cur.HasLabel("bops:landing") {
		return nil
	}
	if _, err := e.BD.Run(ctx, "update", h.id, "--assignee", "", "--status", "open"); err != nil {
		return err
	}
	if _, err := e.BD.Run(ctx, "label", "remove", h.id, "run:"+h.run); err != nil {
		return err
	}
	e.event("sync", h.id, "display_cleared", OutcomeLost, "", nil)
	return nil
}

// allHolds lists every hold in this clone, this worktree's first.
func (e *Env) allHolds() []cloneHold {
	holds := []cloneHold{}
	seen := map[string]bool{}
	for _, id := range e.Run.HeldIDs() {
		seen[id] = true
		holds = append(holds, cloneHold{id: id, run: e.Run.ID, entry: e.Run.Held[id]})
	}
	if e.Store == nil {
		return holds
	}
	for _, h := range e.Store.Holds() {
		if !seen[h.ID] {
			seen[h.ID] = true
			holds = append(holds, cloneHold{id: h.ID, run: h.Run, stranded: h.Stranded, entry: h.Entry})
		}
	}
	return holds
}

// Sync reads the claims, pulls, settles every hold in this clone against its
// ref, repairs the ledger display of this run's holds, and pushes. Another
// run's live hold is checked but not re-displayed or heartbeat: its own run
// does that, and a heartbeat from here would hide that run dying (bops-yr2).
func Sync(ctx context.Context, env *Env, noPush bool) (*SyncOut, error) {
	return syncAs(ctx, env, syncOpts{noPush: noPush, name: "sync"})
}

// syncOpts shape one sync. name is the command its bead-lock lines carry, so
// session-start's are told apart from sync's (bops-7uu). With ownOnly it
// settles only this run's holds and writes nothing for any other's, only
// listing a stranded one: session-start runs on every session start in any
// checkout, so it must not touch work another run owns (bops-yr2). A
// pullReserve above 0 ends the pull that long before ctx's deadline, so the
// caller has time left to report (bops-9rb).
type syncOpts struct {
	noPush      bool
	name        string
	ownOnly     bool
	pullReserve time.Duration
}

func syncAs(ctx context.Context, env *Env, o syncOpts) (*SyncOut, error) {
	noPush, name, ownOnly := o.noPush, o.name, o.ownOnly
	out := &SyncOut{StrandedHolds: []string{}, LostClaims: []string{}, Deferred: []string{}}
	// The holds are read before the refs, so each hold's entry is no newer
	// than the ref it is checked against (bops-yph).
	holds := env.allHolds()
	if ownOnly {
		mine := holds[:0:0]
		for _, h := range holds {
			switch {
			case h.run == env.Run.ID:
				mine = append(mine, h)
			case h.stranded:
				out.StrandedHolds = append(out.StrandedHolds, h.id)
			}
		}
		holds = mine
	}
	refs := map[string]lock.Ref{}
	if env.LockOff {
		out.Warnings = append(out.Warnings, lock.OffWarning)
	} else {
		if err := env.Lock.Fetch(ctx); err != nil {
			return out, err
		}
		seen, err := env.Lock.Seen(ctx)
		if err != nil {
			return out, err
		}
		for _, r := range seen {
			refs[r.ID] = r
		}
		if err := env.noLedgerRemote(ctx); err != nil {
			return out, err // the cause, not bd's "no remote" from the pull
		}
	}
	publish := !env.unpublished(ctx) // pulled and pushed stay false when it is not
	if !publish {
		out.Warnings = append(out.Warnings, noDoltRemoteWarning)
	} else if _, err := pull(ctx, env, o.pullReserve); err != nil {
		return out, err
	} else {
		out.Pulled = true
	}
	for _, h := range holds {
		env.syncHeld(ctx, name, h, refs, out)
	}
	env.pruneBeadLocks(name)
	if noPush || !publish {
		return out, nil
	}
	if err := env.pushLedger(ctx); err != nil {
		return out, err
	}
	out.Pushed = true
	return out, nil
}

// pull runs bd dolt pull, ending it reserve before ctx's deadline when
// reserve is set. A pull cut off that way says so and names bops sync, whose
// budget is longer, rather than bd's bare "signal: terminated".
func pull(ctx context.Context, env *Env, reserve time.Duration) ([]byte, error) {
	dl, ok := ctx.Deadline()
	if reserve <= 0 || !ok {
		return withLockRetry(ctx, env, "dolt", "pull")
	}
	given := time.Until(dl.Add(-reserve)).Round(time.Second)
	pctx, cancel := context.WithDeadline(ctx, dl.Add(-reserve))
	defer cancel()
	b, err := withLockRetry(pctx, env, "dolt", "pull")
	if err != nil && pctx.Err() != nil && ctx.Err() == nil {
		return b, fmt.Errorf("bd dolt pull did not finish in the %s session-start gives it; run `bops sync`, which allows 2 minutes: %w", given, err)
	}
	return b, err
}

// pruneBeadLocks removes the lock files of beads no run in this clone holds,
// so the state directory does not keep one per bead ever touched (bops-exb).
// It is housekeeping: it never waits, and a failure costs only disk space.
func (e *Env) pruneBeadLocks(cmd string) {
	if e.Store == nil {
		return
	}
	n, err := e.Store.PruneBeadLocks()
	if err != nil || n > 0 {
		ev := runstate.Event{At: e.now(), Run: e.Run.ID, Command: cmd, Step: "bead_locks_pruned", Outcome: "pruned", Detail: fmt.Sprintf("%d files", n)}
		if err != nil {
			ev.Outcome, ev.Code, ev.Detail = OutcomeError, lock.CodeOf(err), fmt.Sprintf("%d files; %s", n, lock.Scrub(err.Error()))
		}
		e.Store.Log(ev)
	}
}

// syncHeld settles one hold while holding the bead's lock, so no other bops
// process sharing this clone changes it meanwhile. A hold another process is
// working on, or one whose entry changed since this sync read the holds (so
// the refs fetched after may predate it), is left for the next sync
// (bops-yph).
func (e *Env) syncHeld(ctx context.Context, name string, h cloneHold, refs map[string]lock.Ref, out *SyncOut) {
	unlock, err := e.lockBead(ctx, name, h.id, &out.Warnings, 0)
	if lock.CodeOf(err) == lock.CodeContention {
		out.Deferred = append(out.Deferred, h.id)
		e.event(name, h.id, "bead_lock", eventDeferred, "", nil)
		return
	}
	if err != nil {
		out.Warnings = append(out.Warnings, h.id+": "+err.Error())
		return
	}
	defer unlock()
	if e.Store != nil {
		if !e.Store.Matches(h.run, h.id, h.entry) {
			out.Deferred = append(out.Deferred, h.id)
			e.event(name, h.id, "entry_changed", eventDeferred, "", nil)
			return
		}
	}
	if e.syncHold(ctx, h, refs, out) {
		// Under the lock, so a bead released meanwhile is not heartbeat.
		_, _ = e.BD.Run(ctx, "heartbeat", h.id) // best effort; the ref decides ownership
	}
}

// syncHold settles one hold and reports whether it is still live here. The
// order matters: an unfinished write or delete must be settled before an
// absent ref can be read as a lost claim.
func (e *Env) syncHold(ctx context.Context, h cloneHold, refs map[string]lock.Ref, out *SyncOut) bool {
	mine := h.run == e.Run.ID // only this run's live holds are re-displayed and heartbeat (bops-yr2)
	if e.LockOff {            // no ref to settle against, so no hold is lost here
		if h.stranded {
			out.StrandedHolds = append(out.StrandedHolds, h.id)
			return false
		}
		return mine
	}
	ref, found := refs[h.id]
	live := ""
	if found {
		live = ref.SHA
	}
	if h.entry.Intended != "" || h.entry.Phase == runstate.PhasePushing {
		// The refs were fetched before this bead's lock, while its writer
		// may still have been pushing; a writer that died since freed the
		// lock with its push landed. Settle against the ref as it is now.
		now, err := e.Lock.Read(ctx, h.id)
		if err != nil {
			out.Warnings = append(out.Warnings, h.id+": "+err.Error())
			return false
		}
		ref, found, live = lock.Ref{}, now != nil, ""
		if found {
			ref, live = *now, now.SHA
		}
		state, err := e.Lock.Settle(h.id, live)
		if err != nil {
			out.Warnings = append(out.Warnings, h.id+": "+err.Error())
			return false
		}
		if state == lock.Discarded {
			return false
		}
		if state == lock.Adopted && ref.State == lock.StateLanding {
			// A release --reason done that pushed its landing and died before
			// dropping the hold: the bead is a landing now, which sweep closes.
			if err := e.Lock.Journal.Drop(h.id); err != nil {
				out.Warnings = append(out.Warnings, h.id+": "+err.Error())
			}
			e.event("sync", h.id, "ref_check", "landing", live, nil)
			return false
		}
		h.entry, _ = e.Lock.Journal.Entry(h.id)
	}
	if h.entry.DeletePending {
		if _, err := e.Lock.Delete(ctx, h.id, h.entry.SHA); err != nil {
			out.Warnings = append(out.Warnings, h.id+": ref still not removed: "+err.Error())
		}
		return false
	}
	if h.stranded {
		out.StrandedHolds = append(out.StrandedHolds, h.id)
		return false
	}
	if h.entry.Legacy() {
		return mine
	}
	if live != h.entry.SHA {
		out.LostClaims = append(out.LostClaims, h.id)
		e.event("sync", h.id, "ref_check", OutcomeLost, live, nil)
		if err := e.Lock.Journal.Drop(h.id); err != nil {
			out.Warnings = append(out.Warnings, h.id+": "+err.Error())
		}
		if !found {
			if err := e.clearOwnDisplay(ctx, h); err != nil {
				out.Warnings = append(out.Warnings, h.id+": claim display not cleared: "+err.Error())
			}
		}
		return false
	}
	if !mine {
		return false // live, and its own run's sync repairs its display
	}
	cur, err := e.show(ctx, h.id)
	if err == nil {
		err = e.display(ctx, cur, ref.Actor, ref.Run, h.entry.LedgerPending, true)
	}
	if err != nil {
		out.Warnings = append(out.Warnings, h.id+": ledger display pending: "+err.Error())
		e.setLedgerPending(h.id, true)
		return true
	}
	if h.entry.LedgerPending {
		e.setLedgerPending(h.id, false)
	}
	return true
}
