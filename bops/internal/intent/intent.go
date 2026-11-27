// Package intent implements the commands in commands.json on top of a
// bdx.Runner. Every function is deterministic given the runner's replies,
// so tests drive them with bdx.Fake.
package intent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// Env is everything an intent needs besides its arguments.
type Env struct {
	BD  bdx.Runner
	Run *runstate.Run
	// Store is the clone's hold state across worktrees. Nil means only Run.
	Store HoldStore
	// Lock decides who holds a bead. Next works without it; writes need it.
	// With LockOff its journal still records holds, but no ref is read or
	// written.
	Lock *lock.Lock
	// LockOff is lock.mode none: ownership is decided by the ledger alone
	// (bd update --claim, the assignee and run: label, updated_at), with no
	// git ref and no network for locking. Only for a ledger on one machine.
	LockOff bool
	Now     func() time.Time
	Sleep   func(time.Duration) // injectable for the sync retry
	// BeadWait is how long a command waits for a bead another bops process on
	// this machine is working on before it answers ref_contention. sync and
	// sweep never wait; they leave such a bead for their next run (bops-yph).
	BeadWait time.Duration
	// PullReserve is how much of session-start's budget its pull must leave
	// for the rest of the document; 0 means DefaultPullReserve (bops-9rb).
	PullReserve time.Duration
	// StaleAfter is how long a claim may go without a heartbeat/update
	// before `next` treats it as stale and `claim --force` may take it
	// (config stale_minutes). Default 2h.
	StaleAfter time.Duration
	// HoldFor is how long bops:hold keeps a bead out of `next` (config
	// hold_minutes). Zero means the 120 minute default.
	HoldFor time.Duration
	// BlockingLabels (config blocking_labels) keep a bead out of `next` (W15)
	// and make a plain claim refuse it.
	BlockingLabels []string
}

// HoldStore is the part of the clone's state that spans worktrees.
type HoldStore interface {
	Holds() []runstate.Hold
	SetLedgerPending(id string, pending bool) error
	SetReleasePending(id string, pending bool) error
	Log(ev runstate.Event)
	// LockBead, Refresh and Matches serialize work on one bead among the
	// bops processes sharing this clone (bops-yph).
	LockBead(id string) (func(), error)
	Refresh() error
	Matches(run, id string, expect runstate.Entry) bool
	// PruneBeadLocks removes the lock files of beads no run holds (bops-exb).
	PruneBeadLocks() (int, error)
}

// holdBead takes this clone's lock on id for the rest of a command and
// re-reads the state file, so what the command then reads of the bead is
// current and no other bops process on this machine changes it until the
// returned function runs. Every worktree of a clone, and every session in one
// checkout, shares the state file; a session in the main checkout shares the
// /bead-loop orchestrator's run (bops-yph). A held lock is ref_contention.
// Where the state directory's file system has no file locks, the command runs
// unserialized, as bops did before, and warns once.
func (e *Env) holdBead(ctx context.Context, cmd, id string, warnings *[]string) (func(), error) {
	return e.lockBead(ctx, cmd, id, warnings, e.BeadWait)
}

// beadPoll is how often a waiting command tries a busy bead's lock again.
const beadPoll = 200 * time.Millisecond

func (e *Env) lockBead(ctx context.Context, cmd, id string, warnings *[]string, wait time.Duration) (func(), error) {
	if e.Store == nil {
		return func() {}, nil
	}
	start := time.Now() // the real clock: these are durations, not timestamps
	unlock, err := e.Store.LockBead(id)
	for deadline := start.Add(wait); errors.Is(err, runstate.ErrBeadBusy) && time.Now().Before(deadline) && ctx.Err() == nil; {
		e.sleepFor(beadPoll)
		unlock, err = e.Store.LockBead(id)
	}
	held := time.Now()
	waited := held.Sub(start)
	if errors.Is(err, runstate.ErrBeadBusy) {
		if wait > 0 { // sync and sweep, which do not wait, log a deferral instead
			// timeout: the command's own time ran out first, so this wait
			// says less about how long the holder kept the bead than busy.
			outcome := "busy"
			if ctx.Err() != nil {
				outcome = "timeout"
			}
			e.lockEvent(cmd, id, outcome, &waited, nil)
		}
		return nil, lock.BusyError(id)
	}
	if errors.Is(err, runstate.ErrBeadLockUnsupported) {
		if !slices.Contains(*warnings, noBeadLockWarning) {
			*warnings = append(*warnings, noBeadLockWarning)
		}
		if err := e.Store.Refresh(); err != nil {
			return nil, err
		}
		return func() {}, nil // no lock, so nothing to time
	}
	if err != nil {
		return nil, err
	}
	if err := e.Store.Refresh(); err != nil {
		d := time.Since(held)
		e.lockEvent(cmd, id, "refresh_failed", &waited, &d)
		unlock()
		return nil, err
	}
	return func() {
		// Logged while still held, so the line precedes the next holder's.
		d := time.Since(held)
		e.lockEvent(cmd, id, "released", &waited, &d)
		unlock()
	}, nil
}

// lockEvent logs how long a command waited for a bead's lock and held it, so
// BeadWait can be sized from what commands really do (bops-7uu).
func (e *Env) lockEvent(cmd, id, outcome string, waited, held *time.Duration) {
	ms := func(d *time.Duration) *int64 {
		if d == nil {
			return nil
		}
		v := d.Milliseconds()
		return &v
	}
	e.Store.Log(runstate.Event{At: e.now(), Run: e.Run.ID, Command: cmd, Bead: id, Step: "bead_lock", Outcome: outcome,
		WaitedMS: ms(waited), HeldMS: ms(held)})
}

// errReleasePush marks a pending release whose ledger push failed.
var errReleasePush = errors.New("ledger push failed")

// finishRelease finishes a release of id whose ref delete never completed,
// as sync would, before a command acts on the bead: such a hold was given
// away, and treating it as held would take the bead back (bops-yph). A
// release whose ledger push has not landed (reap's) is pushed first, and its
// ref deleted only once the push has. It reports whether it removed the hold.
// The caller holds the bead's lock.
func (e *Env) finishRelease(ctx context.Context, id string) (bool, error) {
	finished, _, err := e.settleRelease(ctx, id)
	return finished, err
}

// settleRelease is finishRelease that also returns, for a pending release
// whose ref another run took meanwhile, that run as the holder: the hold is
// dropped and nothing is pushed or deleted for it.
func (e *Env) settleRelease(ctx context.Context, id string) (bool, *Holder, error) {
	if e.LockOff || e.Lock == nil {
		return false, nil, nil
	}
	h, ok := e.Lock.Journal.Entry(id)
	if !ok || !h.Given() {
		return false, nil, nil
	}
	if h.ReleasePending && !h.DeletePending {
		ref, err := e.Lock.Read(ctx, id)
		if err != nil {
			return false, nil, err
		}
		if ref == nil || ref.SHA != h.SHA {
			if err := e.Lock.Journal.Drop(id); err != nil {
				return false, nil, err
			}
			e.event("release", id, "ref_check", OutcomeLost, "", nil)
			holder := e.holder(ref)
			if holder == nil {
				holder = &Holder{}
			}
			return true, holder, nil
		}
		if err := e.pushLedger(ctx); err != nil {
			return false, nil, fmt.Errorf("a release of %s is on this machine's ledger but not on the remote yet, so its claim ref stays until a ledger push lands: %w: %w", id, errReleasePush, err)
		}
		e.event("release", id, "ledger", "pushed", h.SHA, nil)
	}
	if _, err := e.Lock.Delete(ctx, id, h.SHA); err != nil {
		return false, nil, err
	}
	e.event("release", id, "ref_delete", "finished", h.SHA, nil)
	return true, nil, nil
}

func (e *Env) sleepFor(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

// holds is every hold of this clone, as the state file read last has them.
func (e *Env) holds() []runstate.Hold {
	if e.Store == nil {
		return nil
	}
	return e.Store.Holds()
}

// noBeadLockWarning is holdBead's warning where file locks are missing.
const noBeadLockWarning = "bead lock unavailable: the bops state directory's file system has no file locks, so two bops processes on this machine could work on one bead at once"

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Env) staleAfter() time.Duration {
	if e.StaleAfter == 0 {
		return 2 * time.Hour
	}
	return e.StaleAfter
}

func (e *Env) caps() map[string]bool {
	m := map[string]bool{}
	for _, c := range e.Run.Caps {
		m[c] = true
	}
	return m
}

// ErrPrecondition means a write refused to run because it could not
// verify its precondition (exit 1, fail closed).
var ErrPrecondition = errors.New("precondition not met")

// ---------------------------------------------------------------- next

// Candidate is one `next` result.
type Candidate struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Priority       int      `json:"priority"`
	Type           string   `json:"type"`
	Workable       bool     `json:"workable"`
	Why            []string `json:"why"`
	WhyNot         []string `json:"why_not"`
	BlockedByGates []string `json:"blocked_by_gates"`
	Parent         string   `json:"parent,omitempty"`
	// Touches is the paths the bead declares with touches: labels.
	Touches []string `json:"touches,omitempty"`
}

// NextOut is the `next` output.
type NextOut struct {
	Candidates     []Candidate `json:"candidates"`
	ExcludedCount  int         `json:"excluded_count"`
	LedgerRevision string      `json:"ledger_revision"`
	Warnings       []string    `json:"warnings,omitempty"`
}

// NextOpts are the `next` arguments.
type NextOpts struct {
	N                 int
	IncludeUnworkable bool
	// Local skips refreshing the claim refs from the lock remote, for a caller
	// that has just fetched them (session-start, after its sync).
	Local bool
	// AvoidConflicts excludes a candidate whose touches: paths overlap a
	// live claim's (advisory; a bead that declares none never conflicts).
	AvoidConflicts bool
}

// Next implements SPEC.md §5: filter `bd ready` through Lint and the claim
// refs (W14), then order.
func Next(ctx context.Context, env *Env, o NextOpts) (*NextOut, error) {
	if o.N <= 0 {
		o.N = 3
	}
	raw, err := env.BD.Run(ctx, "ready", "--limit", "0", "--json")
	if err != nil {
		return nil, err
	}
	ready, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	// Parents with an in-progress sibling come first in the ordering.
	inProgress, listErr := listInProgress(ctx, env)
	activeParents := map[string]bool{}
	for _, i := range inProgress {
		if i.Parent != "" {
			activeParents[i.Parent] = true
		}
	}
	lenv := LintEnv{Pool: env.Run.Pool, Caps: env.caps(), Now: env.now(), StaleAfter: env.staleAfter(), HoldFor: env.HoldFor, BlockingLabels: env.BlockingLabels}
	out := &NextOut{Candidates: []Candidate{}}
	if listErr != nil { // next still answers: without the list it loses the ordering step and most of the conflict screen
		out.Warnings = append(out.Warnings, "ordering ignores work in progress: "+listErr.Error())
		if o.AvoidConflicts {
			out.Warnings = append(out.Warnings, "conflict screening sees only claimed beads bd ready still lists: "+listErr.Error())
		}
	}
	// bd ready never returns a bead behind an open gate, so W05 can only
	// explain one that next looks up itself: with --include-unworkable, the
	// beads open gates alone hold back. They were never in bd ready, so they
	// do not count in excluded_count.
	gated := map[string]bool{}
	if o.IncludeUnworkable {
		beads, gates, err := gatedBeads(ctx, env)
		if err != nil {
			out.Warnings = append(out.Warnings, "gated beads not listed: "+err.Error())
		}
		lenv.OpenGates = gates
		for _, b := range beads {
			gated[b.ID] = true
		}
		ready = append(ready, beads...)
	}
	claims := map[string]lock.Ref{}
	if env.LockOff {
		out.Warnings = append(out.Warnings, lock.OffWarning)
	} else if env.Lock != nil {
		// The ledger lags the refs: a bead another machine has claimed reads
		// open here until its ledger push lands and this clone pulls. One
		// fetch brings every claim ref, with its holder, so W14 skips it.
		if !o.Local {
			if err := env.Lock.Fetch(ctx); err != nil {
				out.Warnings = append(out.Warnings, "claim refs not refreshed, W14 uses the last sync's copy: "+err.Error())
			}
		}
		seen, err := env.Lock.Seen(ctx)
		if err != nil {
			out.Warnings = append(out.Warnings, "could not read local claims: "+err.Error())
		}
		for _, r := range seen {
			claims[r.ID] = r
		}
	}
	var live map[string][]string
	var liveOrder []string
	if o.AvoidConflicts {
		live = liveTouches(inProgress, ready, claims)
		for id := range live {
			liveOrder = append(liveOrder, id)
		}
		sort.Strings(liveOrder)
	}
	type scored struct {
		c Candidate
		i ledger.Issue
	}
	var all []scored
	for _, i := range ready {
		if i.IssueType == "epic" {
			out.ExcludedCount++
			if !o.IncludeUnworkable {
				continue
			}
		}
		lr := Lint(&i, lenv)
		c := Candidate{ID: i.ID, Title: i.Title, Priority: i.Priority, Type: i.IssueType, Workable: lr.Workable,
			Why: lr.Passes, WhyNot: []string{}, BlockedByGates: []string{}, Parent: i.Parent, Touches: touches(&i)}
		if i.IssueType == "epic" {
			c.Workable = false
			c.WhyNot = append(c.WhyNot, "epic: agents work leaves, not epics")
		}
		for _, f := range lr.Fails {
			c.WhyNot = append(c.WhyNot, f.ID+": "+f.Rule)
			if f.ID == "W05" {
				c.BlockedByGates = lenv.OpenGates[i.ID]
			}
		}
		for _, w := range lr.Warns {
			c.Why = append(c.Why, w.ID+" (warn): "+w.Rule)
		}
		if r, ok := claims[i.ID]; ok && r.Run != env.Run.ID {
			c.Workable = false
			c.WhyNot = append(c.WhyNot, fmt.Sprintf("W14: claimed by %s", r.Actor))
		}
		if o.AvoidConflicts && len(c.Touches) > 0 {
			if why := conflict(i.ID, c.Touches, live, liveOrder); why != "" {
				c.Workable = false
				c.WhyNot = append(c.WhyNot, why)
			}
		}
		if !c.Workable {
			if i.IssueType != "epic" && !gated[i.ID] {
				out.ExcludedCount++
			}
			if !o.IncludeUnworkable {
				continue
			}
		}
		all = append(all, scored{c, i})
	}
	sort.SliceStable(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if x.c.Workable != y.c.Workable {
			return x.c.Workable
		}
		if x.i.Priority != y.i.Priority {
			return x.i.Priority < y.i.Priority
		}
		xa, ya := activeParents[x.i.Parent], activeParents[y.i.Parent]
		if xa != ya {
			return xa
		}
		if !x.i.CreatedAt.Equal(y.i.CreatedAt) {
			return x.i.CreatedAt.Before(y.i.CreatedAt)
		}
		return x.i.ID < y.i.ID
	})
	for k, s := range all {
		if k >= o.N {
			break
		}
		out.Candidates = append(out.Candidates, s.c)
	}
	return out, nil
}

// listInProgress is every in-progress bead: their parents order next, and
// their touches: paths screen conflicts.
func listInProgress(ctx context.Context, env *Env) ([]ledger.Issue, error) {
	raw, err := env.BD.Run(ctx, "list", "--status", "in_progress", "--limit", "0", "--json")
	if err != nil {
		return nil, err
	}
	return ledger.DecodeIssues(raw)
}

// ---------------------------------------------------------------- note

// NoteOut is the `note` output.
type NoteOut struct {
	ID        string   `json:"id"`
	Outcome   string   `json:"outcome"`
	CommentID string   `json:"comment_id,omitempty"`
	Warnings  []string `json:"warnings"`
	Holder    *Holder  `json:"holder,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	SavedTo   string   `json:"saved_to,omitempty"` // where a refused note's text went
	OpID      string   `json:"op_id,omitempty"`
	Replayed  bool     `json:"replayed,omitempty"` // op_id was already recorded; nothing written
}

// Negative reports a run that worked and an answer of no.
func (o *NoteOut) Negative() bool { return o != nil && negative(o.Outcome) }

// OutcomeNoted and OutcomeLanded are the successes of note and land.
const (
	OutcomeNoted  = "noted"
	OutcomeLanded = "landed"
)

// noteEndings are the prefixes a note's last paragraph should start with,
// so a fresh agent can resume from it (W09).
var noteEndings = []string{"NEXT:", "BLOCKER:", "KEY DECISION:", "REF:"}

// Note records where this run has got to and keeps the claim. The text goes
// to bd through a private file, never as a command-line argument.
func Note(ctx context.Context, env *Env, id, text string) (*NoteOut, error) {
	return NoteOp(ctx, env, id, text, "")
}

// NoteOp is Note with an operation id (see opid.go); "" derives one from the
// run and text. A note whose op id this run already recorded on the bead
// writes no comment and sets replayed: while this run holds the bead it still
// refreshes the claim and heartbeats (noted); otherwise it answers refused or
// lost.
func NoteOp(ctx context.Context, env *Env, id, text, op string) (*NoteOut, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: note text is empty", ErrPrecondition)
	}
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	op, err := opID(op, "note", id, env.Run.ID, text)
	if err != nil {
		return nil, err
	}
	out := &NoteOut{ID: id, Warnings: []string{}, OpID: op}
	// While another process works on the bead, this run cannot tell whether
	// it still holds it, so the note waits; its text is kept at saved_to.
	unlock, err := env.holdBead(ctx, "note", id, &out.Warnings)
	if err != nil {
		return out.refuse(OutcomeError, err.Error(), nil, text), err
	}
	defer unlock()
	// A repeat finds its comment before the hold check: the hold may have
	// gone since the note landed. The answer is then refused or lost, as
	// for any note, with replayed set: the text is on the bead already.
	rec, err := env.findOp(ctx, id, evNote, op)
	if err != nil {
		out.Warnings = append(out.Warnings, "earlier notes not checked for op "+op+": "+lock.Scrub(err.Error()))
	}
	if _, err := env.finishRelease(ctx, id); err != nil {
		return out.refuse(OutcomeError, err.Error(), nil, text), err
	}
	_, held := env.Run.Held[id]
	if rec != nil {
		out.Outcome, out.Replayed, out.CommentID = OutcomeNoted, true, string(rec.comment.ID)
		out.Warnings = append(out.Warnings, replayNote(op))
		if !held {
			out.Outcome, out.Reason = OutcomeRefused, id+" is not held by this run"
			return out, nil
		}
	} else if !held {
		return out.refuse(OutcomeRefused, id+" is not held by this run", nil, text), nil
	}
	// A network failure is only a warning, because a note must work offline.
	// A replay still keeps the claim alive, as a note does; it writes no comment.
	switch r := env.refreshRef(ctx, "note", id, OutcomeNoted, &out.Warnings); r.outcome {
	case refreshLost:
		if out.Replayed {
			out.Outcome, out.Holder, out.Reason = OutcomeLost, r.holder, id+" is no longer held by this run"
			return out, nil
		}
		out.refuse(OutcomeLost, id+" is no longer held by this run", r.holder, text)
		return out, nil
	case refreshFailed:
		out.Warnings = append(out.Warnings, "claim not refreshed: "+r.err.Error())
	}
	if out.Replayed {
		if _, err := env.BD.Run(ctx, "heartbeat", id); err != nil {
			out.Warnings = append(out.Warnings, "heartbeat failed: "+err.Error())
		}
		return out, nil
	}
	path, cleanup, err := writeTemp(notePrefix + env.Run.ID + " op=" + op + "\n" + text)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	added, err := env.BD.Run(ctx, "comments", "add", id, "-f", path, "--json")
	if err != nil {
		return nil, err
	}
	out.Outcome = OutcomeNoted
	if out.CommentID, err = ledger.DecodeCommentID(added); err != nil {
		out.Warnings = append(out.Warnings, "note saved, but its comment id is unknown: "+err.Error())
	}
	if !endsWithHandoff(text) {
		out.Warnings = append(out.Warnings, "W09: last paragraph should start with "+strings.Join(noteEndings, " "))
	}
	// The comment is already saved, so a lost lease is reported, not fatal.
	if _, err := env.BD.Run(ctx, "heartbeat", id); err != nil {
		out.Warnings = append(out.Warnings, "heartbeat failed: "+err.Error())
	}
	return out, nil
}

// refuse records why a note was not applied and keeps its text, so the work
// it describes is not lost with the claim.
func (o *NoteOut) refuse(outcome, why string, holder *Holder, text string) *NoteOut {
	o.Outcome, o.Reason, o.Holder = outcome, why, holder
	o.SavedTo = saveText("note", text, &o.Warnings)
	return o
}

// saveText writes the text of a command that did not run to a private temp
// file and returns its path, so text from stdin is not lost; a failure is a
// warning and an empty path.
func saveText(what, text string, warnings *[]string) string {
	f, err := os.CreateTemp("", "bops-"+what+"-*") // created mode 0600
	if err != nil {
		*warnings = append(*warnings, what+" text could not be saved: "+err.Error())
		return ""
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		*warnings = append(*warnings, what+" text could not be saved: "+err.Error())
		return ""
	}
	return f.Name()
}

// lockHeld takes id's bead lock for the rest of a command (see holdBead),
// finishes a release whose ref delete never completed, and reports whether
// this run holds the bead. Unless it returns held and no error, it has let
// the lock go already; otherwise the caller runs unlock when done.
func (e *Env) lockHeld(ctx context.Context, cmd, id string, warnings *[]string) (unlock func(), held bool, err error) {
	unlock, err = e.holdBead(ctx, cmd, id, warnings)
	if err != nil {
		return nil, false, err
	}
	if _, err := e.finishRelease(ctx, id); err != nil {
		unlock()
		return nil, false, err
	}
	if _, held := e.Run.Held[id]; !held {
		unlock()
		return nil, false, nil
	}
	return unlock, true, nil
}

// What refreshRef did to a claim's ref.
const (
	refreshDone   = "refreshed" // replaced; its at is now
	refreshLost   = "lost"      // the ref is another run's now; the hold is dropped
	refreshFailed = "failed"    // not replaced (err says why); the hold is kept
	refreshNoRef  = "no_ref"    // lock.mode none: there is no ref
	refreshLegacy = "legacy"    // a hold taken before the lock, with no ref
)

// refreshResult is what refreshRef found.
type refreshResult struct {
	outcome string
	holder  *Holder // who holds the bead now, when lost
	err     error   // why the ref was not replaced, when failed
}

// refreshRef replaces this run's claim ref with a new commit, leasing on the
// sha it holds, so the ref's at (the claim's age for stale, reap and claim
// --force) shows activity (D27). It writes nothing to the ledger. A ref that
// is another run's now drops the hold. The caller holds the bead's lock and
// has checked that this run holds the bead (note inline, renew through
// lockHeld). note and renew share it; done is the outcome the event log records for a refresh.
func (e *Env) refreshRef(ctx context.Context, cmd, id, done string, warnings *[]string) refreshResult {
	if e.LockOff || e.Lock == nil {
		return refreshResult{outcome: refreshNoRef}
	}
	entry, owned, err := e.checkHold(ctx, id)
	if err != nil {
		return refreshResult{outcome: refreshFailed, err: err}
	}
	lost := func(holder *Holder) refreshResult {
		e.event(cmd, id, "ref_refresh", OutcomeLost, "", nil)
		return refreshResult{outcome: refreshLost, holder: holder}
	}
	if !owned {
		holder, err := e.dropLost(ctx, id)
		if err != nil {
			*warnings = append(*warnings, err.Error())
		}
		return lost(holder)
	}
	if entry.Legacy() {
		return refreshResult{outcome: refreshLegacy}
	}
	res, err := e.Lock.Replace(ctx, id, entry.SHA, lock.StateHeld)
	if err != nil {
		e.event(cmd, id, "ref_refresh", "pending", entry.SHA, err)
		return refreshResult{outcome: refreshFailed, err: err}
	}
	*warnings = append(*warnings, prefixed(id, res.Warnings)...)
	if res.Outcome == lock.Lost {
		if err := e.Lock.Journal.Drop(id); err != nil {
			*warnings = append(*warnings, err.Error())
		}
		return lost(e.holder(res.Holder))
	}
	e.event(cmd, id, "ref_refresh", done, res.SHA, nil)
	return refreshResult{outcome: refreshDone}
}

// writeTemp puts text for bd in a private file, so it stays off the command line.
func writeTemp(body string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "bops-*") // created mode 0600
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	_, err = f.WriteString(body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

func endsWithHandoff(text string) bool {
	paragraphs := strings.Split(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n\n")
	last := strings.TrimSpace(paragraphs[len(paragraphs)-1])
	for _, prefix := range noteEndings {
		if strings.HasPrefix(last, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- land

// contractVersion is recorded in metadata.bops (SPEC §3.3).
const contractVersion = 1

var hexSHA = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// LandOut is the `land` output.
type LandOut struct {
	ID       string        `json:"id"`
	Outcome  string        `json:"outcome"`
	Landed   ledger.Landed `json:"landed,omitzero"`
	Holder   *Holder       `json:"holder,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	OpID     string        `json:"op_id,omitempty"`
	Replayed bool          `json:"replayed,omitempty"` // this landing was already recorded; nothing written
}

// Negative reports a run that worked and an answer of no.
func (o *LandOut) Negative() bool { return o != nil && negative(o.Outcome) }

// LandOpts are the `land` arguments. Verify is nil unless the caller asked
// for --verify, so land touches a forge only on request.
type LandOpts struct {
	Ref, SHA string
	Verify   forge.Forge
	// OpID names the operation (see opid.go); "" derives it from the run,
	// ref and sha.
	OpID string
}

// Land records evidence that work reached its destination. It never merges,
// pushes a branch, or closes the bead.
func Land(ctx context.Context, env *Env, id string, o LandOpts) (*LandOut, error) {
	u, err := url.Parse(o.Ref)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(o.Ref, " \t\r\n") {
		return nil, fmt.Errorf("%w: ref %q is not an http(s) URL", ErrPrecondition, o.Ref)
	}
	if !hexSHA.MatchString(o.SHA) {
		return nil, fmt.Errorf("%w: sha %q is not a lowercase hex commit id", ErrPrecondition, o.SHA)
	}
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	op, err := opID(o.OpID, "land", id, env.Run.ID, o.Ref, o.SHA)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	unlock, err := env.holdBead(ctx, "land", id, &warnings)
	if err != nil {
		return &LandOut{ID: id, Outcome: OutcomeError, OpID: op}, err
	}
	defer unlock()
	if _, err := env.finishRelease(ctx, id); err != nil {
		return &LandOut{ID: id, Outcome: OutcomeError, Warnings: warnings, OpID: op}, err
	}
	if _, held := env.Run.Held[id]; !held {
		// A landing retried after the release that followed it.
		if rec, err := env.findOp(ctx, id, evLanded, op); err != nil {
			warnings = append(warnings, "earlier landings not checked for op "+op+": "+lock.Scrub(err.Error()))
		} else if rec != nil && (rec.fields["ref"] != o.Ref || rec.fields["sha"] != o.SHA) {
			warnings = append(warnings, "op "+op+" was recorded for another ref or sha")
		} else if rec != nil {
			// The answer is the landed record itself, not the comment's copy.
			if l, err := env.landedRecord(ctx, id, rec.fields["ref"], rec.fields["sha"]); err != nil {
				warnings = append(warnings, "earlier landing not confirmed: "+lock.Scrub(err.Error()))
			} else if l != nil {
				out := &LandOut{ID: id, Outcome: OutcomeLanded, Landed: *l, OpID: op, Replayed: true,
					Warnings: append(warnings, replayNote(op))}
				env.republish(ctx, &out.Warnings)
				return out, nil
			}
		}
		return &LandOut{ID: id, Outcome: OutcomeRefused, Reason: id + " is not held by this run", Warnings: warnings, OpID: op}, nil
	}
	if env.LockOff {
		warnings = append(warnings, lock.OffWarning)
	} else {
		_, owned, err := env.checkHold(ctx, id)
		if err != nil {
			return &LandOut{ID: id, Outcome: OutcomeUnreachable, OpID: op}, err
		}
		if !owned {
			holder, err := env.dropLost(ctx, id)
			if err != nil {
				return nil, err
			}
			env.event("land", id, "ref_check", OutcomeLost, "", nil)
			return &LandOut{ID: id, Outcome: OutcomeLost, Holder: holder, Reason: id + " is no longer held by this run", Warnings: warnings, OpID: op}, nil
		}
	}
	cur, err := env.show(ctx, id)
	if err != nil {
		return nil, err
	}
	if env.LockOff {
		if holder := env.ledgerHolder(cur); holder != nil {
			if err := env.Lock.Journal.Drop(id); err != nil {
				return nil, err
			}
			env.event("land", id, "ledger_check", OutcomeLost, "", nil)
			return &LandOut{ID: id, Outcome: OutcomeLost, Holder: holder, Reason: id + " is no longer held by this run", Warnings: warnings, OpID: op}, nil
		}
	}
	if o.Verify != nil {
		head, err := o.Verify.RefSHA(ctx, o.Ref)
		if err != nil {
			return nil, fmt.Errorf("could not verify %s: %w", o.Ref, err)
		}
		if !strings.HasPrefix(head, o.SHA) {
			return nil, fmt.Errorf("%w: %s is at %s, not %s", ErrPrecondition, o.Ref, head, o.SHA)
		}
	}
	entry := ledger.Landed{Ref: o.Ref, SHA: o.SHA, Run: env.Run.ID, At: env.now().Format(time.RFC3339)}
	merged, existing, err := ledger.MergeLanded(cur, entry, contractVersion)
	if err != nil {
		return nil, err
	}
	if existing != nil { // a repeat of the same landing adds no record
		out := &LandOut{ID: id, Outcome: OutcomeLanded, Landed: *existing, OpID: op, Replayed: true,
			Warnings: append(warnings, "this ref and sha are already landed; this is that landing")}
		// This run's landing whose event comment failed gets it now, so a
		// retry after the release can still find it.
		if existing.Run == env.Run.ID {
			if recorded, err := env.landedEvent(ctx, id, o.Ref, o.SHA); err != nil {
				out.Warnings = append(out.Warnings, "landing event not checked: "+lock.Scrub(err.Error()))
			} else if !recorded {
				ev := fmt.Sprintf("%s run=%s ref=%s sha=%s op=%s", evLanded, env.Run.ID, o.Ref, o.SHA, op)
				if _, err := env.BD.Run(ctx, "comments", "add", id, ev); err != nil {
					out.Warnings = append(out.Warnings, "landing event still not recorded: "+lock.Scrub(err.Error()))
				}
			}
		}
		// The first run's push may have failed: publish again.
		env.republish(ctx, &out.Warnings)
		return out, nil
	}
	path, cleanup, err := writeTemp(string(merged))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := env.BD.Run(ctx, "update", id, "--metadata", "@"+path); err != nil {
		return nil, err
	}
	out := &LandOut{ID: id, Outcome: OutcomeLanded, Landed: entry, Warnings: warnings, OpID: op}
	ev := fmt.Sprintf("%s run=%s ref=%s sha=%s op=%s", evLanded, env.Run.ID, o.Ref, o.SHA, op)
	if _, err := env.BD.Run(ctx, "comments", "add", id, ev); err != nil {
		return out, fmt.Errorf("landed but could not record event: %w", err)
	}
	if env.unpublished(ctx) {
		out.Warnings = append(out.Warnings, noDoltRemoteWarning)
		return out, nil
	}
	// bd's push "succeeds" with no remote, so say the landing stayed here.
	if w := env.noLedgerRemoteWarning(ctx); w != "" {
		out.Warnings = append(out.Warnings, w)
		return out, nil
	}
	if err := env.pushLedger(ctx); err != nil {
		return out, fmt.Errorf("landed locally; push failed: %w", err)
	}
	return out, nil
}

// republish pushes the ledger again for a replayed write, whose first run may
// have stopped before its push or had it rejected. The write itself is
// recorded, so every failure is a warning: a replay never fails, offline or
// not, and `bops sync` publishes later.
func (e *Env) republish(ctx context.Context, warnings *[]string) {
	if e.unpublished(ctx) {
		*warnings = append(*warnings, noDoltRemoteWarning)
		return
	}
	if w := e.noLedgerRemoteWarning(ctx); w != "" {
		*warnings = append(*warnings, w)
		return
	}
	if err := e.pushLedger(ctx); err != nil {
		*warnings = append(*warnings, "recorded locally, ledger not published (run bops sync): "+lock.Scrub(err.Error()))
	}
}

// landedEvent reports whether id has this run's bops:landed comment for ref
// and sha, whatever op id it carries.
func (e *Env) landedEvent(ctx context.Context, id, ref, sha string) (bool, error) {
	comments, err := e.comments(ctx, id)
	if err != nil {
		return false, err
	}
	for _, c := range comments {
		if k, f := eventFields(c.Text); k == evLanded && f["run"] == e.Run.ID && f["ref"] == ref && f["sha"] == sha {
			return true, nil
		}
	}
	return false, nil
}

// landedRecord is id's landed entry for ref and sha by this run, or nil.
func (e *Env) landedRecord(ctx context.Context, id, ref, sha string) (*ledger.Landed, error) {
	cur, err := e.show(ctx, id)
	if err != nil {
		return nil, err
	}
	landed, err := cur.Landed()
	if err != nil {
		return nil, err
	}
	for k := range landed {
		if l := landed[k]; l.Ref == ref && l.SHA == sha && l.Run == e.Run.ID {
			return &l, nil
		}
	}
	return nil, nil
}

// --------------------------------------------------------------- sweep

// SweepOut is the `sweep` output.
type SweepOut struct {
	DryRun     bool     `json:"dry_run"`
	Closed     []string `json:"closed"`
	WouldClose []string `json:"would_close"`
	Pending    []string `json:"pending"`
	Unknown    []string `json:"unknown"`
	Warnings   []string `json:"warnings,omitempty"`
}

type sweepCandidate struct {
	issue *ledger.Issue
	last  ledger.Landed
}

// Sweep closes beads whose latest landed ref has merged and deletes their
// locks. It never deletes a lock it did not just read:
//
//	pull --> list --> merged? --> fetch refs --> delete with a lease --> close
//
// The delete comes first on purpose. Its lease is the one point that decides
// between a sweep and a resume. Once it wins no resume can succeed, and closing
// first could leave a closed bead with a live holder.
func Sweep(ctx context.Context, env *Env, f forge.Forge, dryRun bool) (*SweepOut, error) {
	out := &SweepOut{DryRun: dryRun, Closed: []string{}, WouldClose: []string{}, Pending: []string{}, Unknown: []string{}}
	publish := !dryRun
	if publish && env.unpublished(ctx) {
		publish = false
		out.Warnings = append(out.Warnings, noDoltRemoteWarning)
	}
	if publish {
		if err := env.noLedgerRemote(ctx); err != nil {
			return out, err // the cause, not bd's "no remote" from the pull
		}
		if _, err := withLockRetry(ctx, env, "dolt", "pull"); err != nil {
			return out, err
		}
	}
	raw, err := env.BD.Run(ctx, "list", "--status", "in_progress", "--limit", "0", "--json")
	if err != nil {
		return nil, err
	}
	issues, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	merged := []sweepCandidate{}
	for k := range issues {
		i := &issues[k]
		landed, err := i.Landed()
		if err != nil {
			out.Unknown = append(out.Unknown, i.ID)
			continue
		}
		if len(landed) == 0 {
			// A landing with nothing landed has no PR to check; report it rather
			// than skip it, or nothing would (stale and next leave landings out).
			if i.HasLabel("bops:landing") {
				out.Unknown = append(out.Unknown, i.ID)
			}
			continue
		}
		last := landed[len(landed)-1]
		switch f.IsMerged(ctx, last.Ref) {
		case forge.No:
			out.Pending = append(out.Pending, i.ID)
		case forge.Unknown:
			out.Unknown = append(out.Unknown, i.ID)
		case forge.Yes:
			merged = append(merged, sweepCandidate{i, last})
		}
	}
	if env.LockOff {
		out.Warnings = append(out.Warnings, lock.OffWarning)
	}
	if len(merged) == 0 {
		return out, nil
	}
	refs := map[string]lock.Ref{} // with the lock off, every bead takes the no-ref path
	if !env.LockOff {
		if err := env.Lock.Fetch(ctx); err != nil {
			for _, c := range merged {
				out.Unknown = append(out.Unknown, c.issue.ID)
			}
			return out, err
		}
		seen, err := env.Lock.Seen(ctx)
		if err != nil {
			return out, err
		}
		for _, r := range seen {
			refs[r.ID] = r
		}
	}
	var failures []error
	for _, c := range merged {
		if err := env.sweepOne(ctx, c, refs, dryRun, out); err != nil {
			failures = append(failures, err)
		}
	}
	if len(out.Closed) > 0 && publish {
		if err := env.pushLedger(ctx); err != nil {
			failures = append(failures, fmt.Errorf("closed locally; push failed: %w", err))
		}
	}
	return out, errors.Join(failures...)
}

// stillLanding re-reads c's bead and reports whether it is still the landing
// sweep found: in progress, with the same last landed record, and labelled
// bops:landing unless its landing ref was found.
func (e *Env) stillLanding(ctx context.Context, c sweepCandidate, refFound bool) (bool, error) {
	cur, err := e.show(ctx, c.issue.ID)
	if err != nil {
		return false, err
	}
	landed, err := cur.Landed()
	if err != nil {
		return false, err
	}
	return cur.Status == "in_progress" && len(landed) > 0 && landed[len(landed)-1] == c.last &&
		(refFound || cur.HasLabel("bops:landing")), nil
}

func (e *Env) sweepOne(ctx context.Context, c sweepCandidate, refs map[string]lock.Ref, dryRun bool, out *SweepOut) error {
	id := c.issue.ID
	ref, found := refs[id]
	switch {
	case !found && !c.issue.HasLabel("bops:landing"):
		out.Pending = append(out.Pending, id)
		return nil
	case found && (ref.State != lock.StateLanding || ref.Run != c.last.Run):
		out.Pending = append(out.Pending, id) // resumed, or reclaimed since it landed
		return nil
	}
	if dryRun {
		out.WouldClose = append(out.WouldClose, id)
		return nil
	}
	unlock, err := e.lockBead(ctx, "sweep", id, &out.Warnings, 0) // a busy bead waits for the next sweep
	if err != nil {
		out.Unknown = append(out.Unknown, id)
		if lock.CodeOf(err) == lock.CodeContention {
			out.Warnings = append(out.Warnings, id+": "+err.Error())
			e.event("sweep", id, "bead_lock", eventDeferred, "", nil)
			return nil
		}
		return err
	}
	defer unlock()
	// The refs were read before this lock was taken. A bead another session
	// of this clone resumed or re-claimed since holds it at another ref: its
	// hold stands, and sweep leaves it (bops-yph).
	want := ""
	if found {
		want = ref.SHA
	}
	for _, h := range e.holds() {
		if h.ID == id && (h.Entry.SHA != want || h.Entry.Intended != "") {
			out.Pending = append(out.Pending, id)
			e.event("sweep", id, "hold_changed", eventDeferred, want, nil)
			return nil
		}
	}
	// The ledger was read before the lock too: a bead resumed and given back
	// since is no longer this landing, though it has no hold or ref left.
	if still, err := e.stillLanding(ctx, c, found); err != nil {
		out.Unknown = append(out.Unknown, id)
		return err
	} else if !still {
		out.Pending = append(out.Pending, id)
		e.event("sweep", id, "ledger_changed", eventDeferred, want, nil)
		return nil
	}
	if found {
		res, err := e.Lock.Delete(ctx, id, ref.SHA)
		if err != nil {
			out.Unknown = append(out.Unknown, id)
			e.event("sweep", id, "ref_delete", OutcomeUnreachable, ref.SHA, err)
			return err
		}
		if res.Outcome != lock.Gone {
			out.Pending = append(out.Pending, id)
			e.event("sweep", id, "ref_delete", OutcomeLost, ref.SHA, nil)
			return nil
		}
		e.event("sweep", id, "ref_delete", "deleted", ref.SHA, nil)
	}
	reason := fmt.Sprintf("merged via %s (%s)", c.last.Ref, c.last.SHA)
	if _, err := e.BD.Run(ctx, "close", id, "--reason", reason); err != nil {
		out.Unknown = append(out.Unknown, id) // a later sweep finds no ref and closes it
		return err
	}
	out.Closed = append(out.Closed, id)
	e.event("sweep", id, "close", "closed", "", nil)
	// bops:landing displays the landing ref, which is gone now (SPEC §3.1).
	// The close is the outcome that matters, so a label that stays is a
	// warning with the command that removes it, never an unclosed bead.
	if c.issue.HasLabel("bops:landing") {
		if _, err := e.BD.Run(ctx, "label", "remove", id, "bops:landing"); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s closed, but its bops:landing label stayed: run `bd label remove %s bops:landing`: %s", id, id, lock.Scrub(err.Error())))
		}
	}
	return nil
}

func withLockRetry(ctx context.Context, env *Env, args ...string) ([]byte, error) {
	b, err := env.BD.Run(ctx, args...)
	if err != nil && isLockError(err) {
		env.sleepFor(5 * time.Second)
		return env.BD.Run(ctx, args...)
	}
	return b, err
}

// lockErrors are the contention messages in the bd 1.3.4 binary. Matching the
// bare word "lock" also matched "blocked" and "clock".
var lockErrors = []string{
	"database is locked",
	"locked by another dolt process",
	"lock already held by another process",
	"lock timeout exceeded",
}

func isLockError(err error) bool {
	s := strings.ToLower(err.Error())
	for _, m := range lockErrors {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
