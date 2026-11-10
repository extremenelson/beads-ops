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
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// Env is everything an intent needs besides its arguments.
type Env struct {
	BD  bdx.Runner
	Run *runstate.Run
	// Store is the clone's hold state across worktrees. Nil means only Run.
	Store HoldStore
	Now   func() time.Time
	Sleep func(time.Duration) // injectable for the sync retry
	// StaleAfter is how long a claim may go without a heartbeat/update
	// before `next` treats it as stale. Default 2h.
	StaleAfter time.Duration
}

// HoldStore is the part of the clone's state that spans worktrees.
type HoldStore interface {
	Holds() []runstate.Hold
	DropHold(id string)
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
	activeParents := map[string]bool{}
	if ipRaw, err := env.BD.Run(ctx, "list", "--status", "in_progress", "--json"); err == nil {
		if ip, err := ledger.DecodeIssues(ipRaw); err == nil {
			for _, i := range ip {
				if i.Parent != "" {
					activeParents[i.Parent] = true
				}
			}
		}
	}
	lenv := LintEnv{Pool: env.Run.Pool, Caps: env.caps(), Now: env.now(), StaleAfter: env.staleAfter()}
	out := &NextOut{Candidates: []Candidate{}}
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

// --------------------------------------------------------------- claim

// ClaimOut is the `claim` output.
type ClaimOut struct {
	ID         string    `json:"id"`
	Run        string    `json:"run"`
	ClaimedAt  time.Time `json:"claimed_at"`
	ReadbackOK bool      `json:"readback_ok"`
}

// Claim takes a bead for this run: CAS on empty assignee, run label,
// typed comment, readback, push. Any failure before the push leaves the
// ledger as it was except where noted in the returned error.
func Claim(ctx context.Context, env *Env, id string, force bool) (*ClaimOut, error) {
	showRaw, err := env.BD.Run(ctx, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	cur, err := ledger.DecodeIssue(showRaw)
	if err != nil {
		return nil, err
	}
	if !force {
		if cur.Assignee != "" {
			return nil, fmt.Errorf("%w: %s is assigned to %q", ErrPrecondition, id, cur.Assignee)
		}
		if runs := cur.LabelsWithPrefix("run:"); len(runs) > 0 {
			return nil, fmt.Errorf("%w: %s carries run:%s", ErrPrecondition, id, runs[0])
		}
		if cur.Status != "open" && cur.Status != "ready" {
			return nil, fmt.Errorf("%w: %s is %s", ErrPrecondition, id, cur.Status)
		}
	}
	// --claim is bd's own compare-and-set; bd refuses --if-assignee beside it.
	if _, err := env.BD.Run(ctx, "update", id, "--claim"); err != nil {
		return nil, err
	}
	run := env.Run.ID
	if _, err := env.BD.Run(ctx, "label", "add", id, "run:"+run); err != nil {
		return nil, fmt.Errorf("claimed but could not add run label: %w", err)
	}
	if cur.HasLabel("bops:hold") {
		if _, err := env.BD.Run(ctx, "label", "remove", id, "bops:hold"); err != nil {
			return nil, fmt.Errorf("claimed but could not clear bops:hold: %w", err)
		}
	}
	ev := fmt.Sprintf("bops:claimed run=%s actor=%s machine=%s", run, env.Run.Actor, env.Run.Machine)
	if _, err := env.BD.Run(ctx, "comments", "add", id, ev); err != nil {
		return nil, fmt.Errorf("claimed but could not record event: %w", err)
	}
	out := &ClaimOut{ID: id, Run: run, ClaimedAt: env.now()}
	if rb, err := env.BD.Run(ctx, "show", id, "--json"); err == nil {
		if got, err := ledger.DecodeIssue(rb); err == nil {
			out.ReadbackOK = got.Assignee != "" && got.HasLabel("run:"+run)
		}
	}
	if !out.ReadbackOK {
		return out, fmt.Errorf("%w: readback after claim did not show assignee + run label", ErrPrecondition)
	}
	env.Run.Hold(id)
	if _, err := env.BD.Run(ctx, "dolt", "push"); err != nil {
		return out, fmt.Errorf("claimed locally; push failed: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- note

// NoteOut is the `note` output.
type NoteOut struct {
	ID        string   `json:"id"`
	CommentID string   `json:"comment_id"`
	Warnings  []string `json:"warnings"`
}

// noteEndings are the prefixes a note's last paragraph should start with,
// so a fresh agent can resume from it (W09).
var noteEndings = []string{"NEXT:", "BLOCKER:", "KEY DECISION:", "REF:"}

// Note records where this run has got to and keeps the claim. The text goes
// to bd through a private file, never as a command-line argument.
func Note(ctx context.Context, env *Env, id, text string) (*NoteOut, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: note text is empty", ErrPrecondition)
	}
	raw, err := env.BD.Run(ctx, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	cur, err := ledger.DecodeIssue(raw)
	if err != nil {
		return nil, err
	}
	if !cur.HasLabel("run:" + env.Run.ID) {
		return nil, fmt.Errorf("%w: %s is not held by this run", ErrPrecondition, id)
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
	out := &NoteOut{ID: id, Warnings: []string{}}
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
	ID     string        `json:"id"`
	Landed ledger.Landed `json:"landed"`
}

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
	raw, err := env.BD.Run(ctx, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	cur, err := ledger.DecodeIssue(raw)
	if err != nil {
		return nil, err
	}
	if !cur.HasLabel("run:" + env.Run.ID) {
		return nil, fmt.Errorf("%w: %s is not held by this run", ErrPrecondition, id)
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
		return &LandOut{ID: id, Landed: *existing}, nil
	}
	path, cleanup, err := writeTemp(string(merged))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := env.BD.Run(ctx, "update", id, "--metadata", "@"+path); err != nil {
		return nil, err
	}
	out := &LandOut{ID: id, Landed: entry}
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

// Sweep closes beads whose latest landed ref has merged. A bead still
// carrying a run: label is pending: its run has not released it yet.
func Sweep(ctx context.Context, env *Env, f forge.Forge, dryRun bool) (*SweepOut, error) {
	raw, err := env.BD.Run(ctx, "list", "--status", "in_progress", "--limit", "0", "--json")
	if err != nil {
		return nil, err
	}
	issues, err := ledger.DecodeIssues(raw)
	if err != nil {
		return nil, err
	}
	out := &SweepOut{DryRun: dryRun, Closed: []string{}, WouldClose: []string{}, Pending: []string{}, Unknown: []string{}}
	var failures []error
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
		if len(i.LabelsWithPrefix("run:")) > 0 {
			out.Pending = append(out.Pending, i.ID)
			continue
		}
		last := landed[len(landed)-1]
		switch f.IsMerged(ctx, last.Ref) {
		case forge.No:
			out.Pending = append(out.Pending, i.ID)
		case forge.Unknown:
			out.Unknown = append(out.Unknown, i.ID)
		case forge.Yes:
			if dryRun {
				out.WouldClose = append(out.WouldClose, i.ID)
				continue
			}
			reason := fmt.Sprintf("merged via %s (%s)", last.Ref, last.SHA)
			if _, err := env.BD.Run(ctx, "close", i.ID, "--reason", reason); err != nil {
				out.Unknown = append(out.Unknown, i.ID)
				failures = append(failures, err)
				continue
			}
			out.Closed = append(out.Closed, i.ID)
		}
	}
	if len(out.Closed) > 0 {
		if _, err := env.BD.Run(ctx, "dolt", "push"); err != nil {
			failures = append(failures, fmt.Errorf("closed locally; push failed: %w", err))
		}
	}
	return out, errors.Join(failures...)
}

// ------------------------------------------------------------- release

// ReleaseReasons are the allowed `--reason` values.
var ReleaseReasons = []string{"done", "blocked", "scope", "stale", "human"}

// ReleaseOut is the `release` output.
type ReleaseOut struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Release gives a claim back with a classified reason (SPEC §4 effects).
func Release(ctx context.Context, env *Env, id, reason string) (*ReleaseOut, error) {
	if !contains(ReleaseReasons, reason) {
		return nil, fmt.Errorf("release: reason must be one of %s", strings.Join(ReleaseReasons, "|"))
	}
	showRaw, err := env.BD.Run(ctx, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	cur, err := ledger.DecodeIssue(showRaw)
	if err != nil {
		return nil, err
	}
	runs := cur.LabelsWithPrefix("run:")
	if len(runs) > 0 && !contains(runs, env.Run.ID) && reason != "stale" {
		return nil, fmt.Errorf("%w: %s is held by run %s, not this run (use --reason stale to reclaim)", ErrPrecondition, id, runs[0])
	}
	// done stays in_progress until sweep closes it; every other reason
	// returns the bead to a status bd ready understands.
	update := []string{"update", id, "--assignee", ""}
	switch reason {
	case "blocked":
		update = append(update, "--status", "blocked")
	case "human", "scope", "stale":
		update = append(update, "--status", "open")
	}
	if _, err := env.BD.Run(ctx, update...); err != nil {
		return nil, err
	}
	for _, r := range runs {
		if _, err := env.BD.Run(ctx, "label", "remove", id, "run:"+r); err != nil {
			return nil, fmt.Errorf("unassigned but could not remove run label: %w", err)
		}
	}
	label := map[string]string{"human": "needs:human", "scope": "bops:hold", "stale": "bops:hold", "done": "bops:landing"}[reason]
	if label != "" {
		if _, err := env.BD.Run(ctx, "label", "add", id, label); err != nil {
			return nil, err
		}
	}
	ev := fmt.Sprintf("bops:released run=%s reason=%s", env.Run.ID, reason)
	if _, err := env.BD.Run(ctx, "comments", "add", id, ev); err != nil {
		return nil, fmt.Errorf("released but could not record event: %w", err)
	}
	env.Run.Drop(id)
	if env.Store != nil {
		env.Store.DropHold(id) // the hold may belong to a removed worktree's run
	}
	if _, err := env.BD.Run(ctx, "dolt", "push"); err != nil {
		return &ReleaseOut{id, reason}, fmt.Errorf("released locally; push failed: %w", err)
	}
	return &ReleaseOut{id, reason}, nil
}

// ---------------------------------------------------------------- sync

// SyncOut is the `sync` output.
type SyncOut struct {
	Pulled      bool   `json:"pulled"`
	Pushed      bool   `json:"pushed"`
	Revision    string `json:"revision"`
	Unpublished int    `json:"unpublished"`
	// StrandedHolds are beads held by a worktree that no longer exists.
	// `bops release <id> --reason stale` from any checkout clears one.
	StrandedHolds []string `json:"stranded_holds"`
}

// Sync pulls, heartbeats every claim held in this clone, and pushes,
// retrying once after a Dolt lock error (SPEC §4 retry policy).
func Sync(ctx context.Context, env *Env, noPush bool) (*SyncOut, error) {
	held, stranded := env.holds()
	out := &SyncOut{StrandedHolds: stranded}
	if _, err := withLockRetry(ctx, env, "dolt", "pull"); err != nil {
		return out, err
	}
	out.Pulled = true
	for _, id := range held {
		_, _ = env.BD.Run(ctx, "heartbeat", id) // best effort; lease is advisory
	}
	if noPush {
		return out, nil
	}
	if _, err := withLockRetry(ctx, env, "dolt", "push"); err != nil {
		return out, err
	}
	out.Pushed = true
	return out, nil
}

// holds returns every bead held in this clone, and which of them are stranded.
func (e *Env) holds() (held, stranded []string) {
	stranded = []string{}
	seen := map[string]bool{}
	for _, id := range e.Run.Held {
		seen[id] = true
		held = append(held, id)
	}
	if e.Store == nil {
		return held, stranded
	}
	for _, h := range e.Store.Holds() {
		if !seen[h.ID] {
			seen[h.ID] = true
			held = append(held, h.ID)
		}
		if h.Stranded {
			stranded = append(stranded, h.ID)
		}
	}
	return held, stranded
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

func isLockError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "lock")
}
