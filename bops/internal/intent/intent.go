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
	Lock  *lock.Lock
	Now   func() time.Time
	Sleep func(time.Duration) // injectable for the sync retry
	// StaleAfter is how long a claim may go without a heartbeat/update
	// before `next` treats it as stale and `claim --force` may take it
	// (config stale_minutes). Default 2h.
	StaleAfter time.Duration
	// HoldFor is how long bops:hold keeps a bead out of `next` (config
	// hold_minutes). Zero means the 120 minute default.
	HoldFor time.Duration
}

// HoldStore is the part of the clone's state that spans worktrees.
type HoldStore interface {
	Holds() []runstate.Hold
	SetLedgerPending(id string, pending bool) error
	Log(ev runstate.Event)
}

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
}

// Next implements SPEC.md §5: filter `bd ready` through Lint, then order.
func Next(ctx context.Context, env *Env, o NextOpts) (*NextOut, error) {
	if o.N <= 0 {
		o.N = 3
	}
	raw, err := env.BD.Run(ctx, "ready", "--json")
	if err != nil {
		return nil, err
	}
	ready, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	// Parents with an in-progress sibling come first in the ordering.
	activeParents, listErr := parentsInProgress(ctx, env)
	lenv := LintEnv{Pool: env.Run.Pool, Caps: env.caps(), Now: env.now(), StaleAfter: env.staleAfter(), HoldFor: env.HoldFor}
	out := &NextOut{Candidates: []Candidate{}}
	if listErr != nil { // it only affects the order, so a read still answers
		out.Warnings = append(out.Warnings, "ordering ignores work in progress: "+listErr.Error())
	}
	claims := map[string]lock.Ref{}
	if env.Lock != nil {
		seen, err := env.Lock.Seen(ctx)
		if err != nil {
			out.Warnings = append(out.Warnings, "could not read local claims: "+err.Error())
		}
		for _, r := range seen {
			claims[r.ID] = r
		}
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
			Why: lr.Passes, WhyNot: []string{}, BlockedByGates: []string{}, Parent: i.Parent}
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
		if !c.Workable {
			if i.IssueType != "epic" {
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

// parentsInProgress finds the parents that already have work in flight.
func parentsInProgress(ctx context.Context, env *Env) (map[string]bool, error) {
	parents := map[string]bool{}
	raw, err := env.BD.Run(ctx, "list", "--status", "in_progress", "--json")
	if err != nil {
		return parents, err
	}
	issues, err := ledger.DecodeIssues(raw)
	if err != nil {
		return parents, err
	}
	for _, i := range issues {
		if i.Parent != "" {
			parents[i.Parent] = true
		}
	}
	return parents, nil
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
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: note text is empty", ErrPrecondition)
	}
	if err := lock.ValidateID(id); err != nil {
		return nil, err
	}
	out := &NoteOut{ID: id, Warnings: []string{}}
	if _, held := env.Run.Held[id]; !held {
		return out.refuse(OutcomeRefused, id+" is not held by this run", nil, text), nil
	}
	if done := env.refreshClaim(ctx, id, text, out); done {
		return out, nil
	}
	path, cleanup, err := writeTemp(fmt.Sprintf("bops:note run=%s\n%s", env.Run.ID, text))
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
	f, err := os.CreateTemp("", "bops-note-*") // created mode 0600
	if err != nil {
		o.Warnings = append(o.Warnings, "note text could not be saved: "+err.Error())
		return o
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		o.Warnings = append(o.Warnings, "note text could not be saved: "+err.Error())
		return o
	}
	o.SavedTo = f.Name()
	return o
}

// refreshClaim replaces this run's ref so its age shows activity (D27). A
// network failure is only a warning, because a note must work offline. It
// reports true if the note must stop, having lost the claim.
func (e *Env) refreshClaim(ctx context.Context, id, text string, out *NoteOut) bool {
	entry, owned, err := e.checkHold(ctx, id)
	if err != nil {
		out.Warnings = append(out.Warnings, "claim not refreshed: "+err.Error())
		return false
	}
	lost := func(holder *Holder) bool {
		out.refuse(OutcomeLost, id+" is no longer held by this run", holder, text)
		e.event("note", id, "ref_refresh", OutcomeLost, "", nil)
		return true
	}
	if !owned {
		holder, err := e.dropLost(ctx, id)
		if err != nil {
			out.Warnings = append(out.Warnings, err.Error())
		}
		return lost(holder)
	}
	if entry.Legacy() {
		return false
	}
	res, err := e.Lock.Replace(ctx, id, entry.SHA, lock.StateHeld)
	if err != nil {
		out.Warnings = append(out.Warnings, "claim not refreshed: "+err.Error())
		e.event("note", id, "ref_refresh", "pending", entry.SHA, err)
		return false
	}
	if res.Outcome == lock.Lost {
		if err := e.Lock.Journal.Drop(id); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
		}
		return lost(e.holder(res.Holder))
	}
	e.event("note", id, "ref_refresh", OutcomeNoted, res.SHA, nil)
	return false
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
	ID      string        `json:"id"`
	Outcome string        `json:"outcome"`
	Landed  ledger.Landed `json:"landed,omitzero"`
	Holder  *Holder       `json:"holder,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

// Negative reports a run that worked and an answer of no.
func (o *LandOut) Negative() bool { return o != nil && negative(o.Outcome) }

// LandOpts are the `land` arguments. Verify is nil unless the caller asked
// for --verify, so land touches a forge only on request.
type LandOpts struct {
	Ref, SHA string
	Verify   forge.Forge
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
	if _, held := env.Run.Held[id]; !held {
		return &LandOut{ID: id, Outcome: OutcomeRefused, Reason: id + " is not held by this run"}, nil
	}
	_, owned, err := env.checkHold(ctx, id)
	if err != nil {
		return &LandOut{ID: id, Outcome: OutcomeUnreachable}, err
	}
	if !owned {
		holder, err := env.dropLost(ctx, id)
		if err != nil {
			return nil, err
		}
		env.event("land", id, "ref_check", OutcomeLost, "", nil)
		return &LandOut{ID: id, Outcome: OutcomeLost, Holder: holder, Reason: id + " is no longer held by this run"}, nil
	}
	cur, err := env.show(ctx, id)
	if err != nil {
		return nil, err
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
	if existing != nil { // a repeat of the same landing writes nothing
		return &LandOut{ID: id, Outcome: OutcomeLanded, Landed: *existing}, nil
	}
	path, cleanup, err := writeTemp(string(merged))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := env.BD.Run(ctx, "update", id, "--metadata", "@"+path); err != nil {
		return nil, err
	}
	out := &LandOut{ID: id, Outcome: OutcomeLanded, Landed: entry}
	ev := fmt.Sprintf("bops:landed run=%s ref=%s sha=%s", env.Run.ID, o.Ref, o.SHA)
	if _, err := env.BD.Run(ctx, "comments", "add", id, ev); err != nil {
		return out, fmt.Errorf("landed but could not record event: %w", err)
	}
	if _, err := env.BD.Run(ctx, "dolt", "push"); err != nil {
		return out, fmt.Errorf("landed locally; push failed: %w", err)
	}
	return out, nil
}

// --------------------------------------------------------------- sweep

// SweepOut is the `sweep` output.
type SweepOut struct {
	DryRun     bool     `json:"dry_run"`
	Closed     []string `json:"closed"`
	WouldClose []string `json:"would_close"`
	Pending    []string `json:"pending"`
	Unknown    []string `json:"unknown"`
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
	if !dryRun {
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
	if len(merged) == 0 {
		return out, nil
	}
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
	refs := make(map[string]lock.Ref, len(seen))
	for _, r := range seen {
		refs[r.ID] = r
	}
	var failures []error
	for _, c := range merged {
		if err := env.sweepOne(ctx, c, refs, dryRun, out); err != nil {
			failures = append(failures, err)
		}
	}
	if len(out.Closed) > 0 {
		if err := env.pushLedger(ctx); err != nil {
			failures = append(failures, fmt.Errorf("closed locally; push failed: %w", err))
		}
	}
	return out, errors.Join(failures...)
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
	return nil
}

func withLockRetry(ctx context.Context, env *Env, args ...string) ([]byte, error) {
	b, err := env.BD.Run(ctx, args...)
	if err != nil && isLockError(err) {
		sleep := env.Sleep
		if sleep == nil {
			sleep = time.Sleep
		}
		sleep(5 * time.Second)
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
