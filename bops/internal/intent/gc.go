package intent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// GCOpts are the arguments of `gc`.
type GCOpts struct {
	DryRun bool
	// Only, when set, limits gc to these bead ids, normally the ones a
	// preview listed under would_delete. One whose ref is no longer garbage is
	// listed under skipped, which makes the run negative.
	Only []string
}

// Why gc would delete a ref.
const (
	GCClosed = "closed" // the bead is closed
	GCOrphan = "orphan" // a held ref with no claim on the ledger, as stale lists it
)

// GCRef is a claim ref gc deleted, would delete, kept or could not delete.
type GCRef struct {
	ID          string `json:"id"`
	Run         string `json:"run"`
	Actor       string `json:"actor"`
	State       string `json:"state"`
	Why         string `json:"why,omitempty"`
	AgeMinutes  int    `json:"age_minutes"`
	SameMachine bool   `json:"same_machine"`
	Detail      string `json:"detail,omitempty"`
}

// GCOut is the `gc` output.
type GCOut struct {
	DryRun      bool     `json:"dry_run"`
	Deleted     []GCRef  `json:"deleted"`
	WouldDelete []GCRef  `json:"would_delete"`
	Kept        []GCRef  `json:"kept"`
	Failed      []GCRef  `json:"failed"`
	Skipped     []GCRef  `json:"skipped"`
	Warnings    []string `json:"warnings"`
	// namedKept counts ids --only named that a real run kept on its re-check.
	namedKept int
}

// Negative reports a run that left a ref it meant to delete.
func (o *GCOut) Negative() bool {
	return o != nil && (len(o.Failed) > 0 || len(o.Skipped) > 0 || o.namedKept > 0)
}

// gcCandidate is a ref and the bead it names, as gc read them.
type gcCandidate struct {
	ref  lock.Ref
	why  string
	note string // what a person should know, such as a bead missing from the ledger
}

// GC deletes claim refs nothing needs: the ref of every closed bead, and every
// orphan `stale --all-machines` lists (a held ref, older than stale_minutes,
// whose bead shows no claim by that run or actor; a bead missing from the
// ledger counts). Landing refs of beads still in progress are sweep's, and are
// left alone, as is every ref whose bead is open or in progress with a
// matching claim.
//
// A closed bead's ref is kept while it is younger than stale_minutes and was
// written after the bead's last ledger change: that is a claim of a reopened
// bead whose ledger write is still pending, and the ref is the claim.
//
// With DryRun false it pulls the ledger first and refetches the refs, deleting
// nothing if either fails. Each ref is then deleted under the bead's lock,
// after re-reading the bead and this clone's holds, with a lease on the sha gc
// read, so a ref someone re-took since is left alone (listed under failed). A
// ref whose bead changed so it is no longer garbage, or that a hold of this
// clone still names, is listed under kept.
func GC(ctx context.Context, env *Env, o GCOpts) (*GCOut, error) {
	out := &GCOut{DryRun: o.DryRun, Deleted: []GCRef{}, WouldDelete: []GCRef{}, Kept: []GCRef{},
		Failed: []GCRef{}, Skipped: []GCRef{}, Warnings: []string{}}
	if env.LockOff || env.Lock == nil {
		return out, fmt.Errorf("%w: gc needs the claim lock (lock.mode is none)", ErrPrecondition)
	}
	var only []string
	for _, id := range o.Only {
		if err := lock.ValidateID(id); err != nil {
			return out, err
		}
		if !slices.Contains(only, id) {
			only = append(only, id)
		}
	}
	if !o.DryRun {
		// The ledger must be the one claims write: a Dolt remote on another
		// repository would show live claims as missing or closed.
		if err := env.checkRemote(ctx); err != nil {
			return out, err
		}
		if _, err := withLockRetry(ctx, env, "dolt", "pull"); err != nil {
			return out, fmt.Errorf("gc: ledger pull failed, so nothing was deleted: %w", err)
		}
		if err := env.Lock.Fetch(ctx); err != nil {
			return out, fmt.Errorf("gc: claim refs could not be fetched, so nothing was deleted: %w", err)
		}
	}
	st, err := Stale(ctx, env, StaleOpts{AllMachines: true})
	if err != nil {
		return out, err
	}
	out.Warnings = append(out.Warnings, st.Warnings...)
	seen, err := env.Lock.Seen(ctx)
	if err != nil {
		return out, err
	}
	orphans := map[string]bool{}
	for _, r := range st.Orphans {
		orphans[r.ID] = true
	}
	var cands []gcCandidate
	for _, r := range seen {
		if len(only) > 0 && !slices.Contains(only, r.ID) {
			continue
		}
		if ctx.Err() != nil { // a read that ran out of time is no verdict
			out.Failed = append(out.Failed, env.gcRef(nil, r, "not reached: the command's time ran out; run gc again"))
			continue
		}
		c, keep := env.gcJudge(ctx, r, orphans[r.ID])
		switch {
		case ctx.Err() != nil && c == nil:
			out.Failed = append(out.Failed, env.gcRef(nil, r, "not reached: the command's time ran out; run gc again"))
		case strings.HasPrefix(keep, gcUnread):
			out.Failed = append(out.Failed, env.gcRef(nil, r, keep)) // never judged, so never quietly kept
		case keep != "":
			out.Kept = append(out.Kept, env.gcRef(c, r, keep))
			if slices.Contains(only, r.ID) && !o.DryRun {
				out.namedKept++
			}
		case c != nil:
			cands = append(cands, *c)
		}
	}
	for _, id := range only {
		named := func(g GCRef) bool { return g.ID == id }
		if !slices.ContainsFunc(seen, func(r lock.Ref) bool { return r.ID == id }) {
			// Already gone, as after a rerun of a run that was cut short:
			// what was asked for holds, so the run is not negative.
			out.Warnings = append(out.Warnings, id+": no claim ref, so nothing to delete")
			continue
		}
		if !slices.ContainsFunc(cands, func(c gcCandidate) bool { return c.ref.ID == id }) && !slices.ContainsFunc(out.Kept, named) {
			out.Skipped = append(out.Skipped, GCRef{ID: id, Detail: "not a ref gc deletes now; run gc --dry-run again"})
		}
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].ref.ID < cands[b].ref.ID })
	var failures []error
	for k, c := range cands {
		g := env.gcRef(&c, c.ref, c.note)
		if o.DryRun {
			out.WouldDelete = append(out.WouldDelete, g)
			continue
		}
		if ctx.Err() != nil {
			for _, rest := range cands[k:] {
				s := env.gcRef(&rest, rest.ref, "not reached: the command's time ran out; run gc again")
				out.Failed = append(out.Failed, s)
			}
			break
		}
		res, err := env.gcOne(ctx, c, &g, &out.Warnings)
		switch {
		case err != nil:
			if lock.CodeOf(err) != lock.CodeContention { // a busy bead waits for the next gc
				failures = append(failures, fmt.Errorf("%s: %w", c.ref.ID, err))
			}
			out.Failed = append(out.Failed, g)
		case res == gcKept:
			out.Kept = append(out.Kept, g)
			if slices.Contains(only, c.ref.ID) {
				out.namedKept++
			}
		case res == gcLost:
			out.Failed = append(out.Failed, g)
		default:
			out.Deleted = append(out.Deleted, g)
		}
	}
	sort.Slice(out.Kept, func(a, b int) bool { return out.Kept[a].ID < out.Kept[b].ID })
	return out, errors.Join(failures...)
}

// gcResult is what gcOne did with one ref.
type gcResult int

const (
	gcDeleted gcResult = iota
	gcKept             // the re-check found it is no longer garbage
	gcLost             // someone changed the ref since gc read it
)

func (e *Env) gcRef(c *gcCandidate, r lock.Ref, detail string) GCRef {
	g := GCRef{ID: r.ID, Run: r.Run, Actor: r.Actor, State: r.State, Detail: detail,
		SameMachine: e.clonesRuns()[r.Run]}
	if c != nil {
		g.Why = c.why
	}
	if !r.At.IsZero() {
		g.AgeMinutes = int(e.now().Sub(r.At).Minutes())
	}
	return g
}

// gcJudge decides what gc does with ref r. It returns a candidate to delete,
// or a reason to keep r that a person should see, or neither for a ref that
// is simply in use (a claim, or a landing waiting on sweep). orphan says
// stale listed r as an orphan.
func (e *Env) gcJudge(ctx context.Context, r lock.Ref, orphan bool) (*gcCandidate, string) {
	// Stale judged the ref it read; the one read here may be newer. Only an
	// old held ref can be an orphan, so a fresh claim is never taken for one.
	orphan = orphan && e.oldHeld(r)
	if why := e.heldHere(r.ID); why != "" {
		return nil, why
	}
	cur, err := e.show(ctx, r.ID)
	switch {
	case err != nil && orphan && beadMissing(err):
		return &gcCandidate{ref: r, why: GCOrphan, note: "bead not on the ledger"}, ""
	case err != nil && beadMissing(err):
		return nil, "bead not on the ledger, and the ref is not an orphan stale lists (a landing ref, or younger than stale_minutes)"
	case err != nil:
		return nil, gcUnread + lock.Scrub(err.Error())
	}
	return e.gcVerdict(cur, r, orphan)
}

// gcUnread begins the detail of a ref whose bead could not be read.
const gcUnread = "unread: "

// oldHeld reports whether r is held and older than stale_minutes, as stale
// requires of an orphan.
func (e *Env) oldHeld(r lock.Ref) bool {
	return r.State == lock.StateHeld && !r.At.IsZero() && !r.At.After(e.now().Add(-e.staleAfter()))
}

// heldHere says which run of this clone holds id, or "".
func (e *Env) heldHere(id string) string {
	for _, h := range e.holds() {
		if h.ID == id {
			return "held by run " + h.Run + " of this clone: release it or run bops sync from that checkout"
		}
	}
	return ""
}

// gcSkew allows for clocks that differ between the machine that claimed and
// the one that closed: a closed bead's young ref within this of its last
// ledger change is kept, as a claim may be.
const gcSkew = 15 * time.Minute

// gcVerdict is gcJudge's rule for a bead it could read.
func (e *Env) gcVerdict(cur *ledger.Issue, r lock.Ref, orphan bool) (*gcCandidate, string) {
	if cur.Status == "closed" {
		cutoff := e.now().Add(-e.staleAfter())
		switch {
		case r.At.IsZero():
			return nil, "the bead is closed, but the ref's at is unreadable; check it by hand (PROCESS.md, an orphan ref)"
		case r.At.After(cutoff) && r.At.After(cur.UpdatedAt.Add(-gcSkew)):
			return nil, "the bead is closed, but the ref was written after its last ledger change: a claim whose ledger write may be pending"
		}
		return &gcCandidate{ref: r, why: GCClosed}, ""
	}
	if !orphan {
		return nil, ""
	}
	if cur.HasLabel("run:"+r.Run) || (cur.Assignee != "" && cur.Assignee == r.Actor) {
		if cur.Status == "in_progress" {
			return nil, "" // claimed since stale read the ledger
		}
		// A person parked a held bead (blocked, deferred): it is still held,
		// though stale, which reads in-progress beads only, lists its ref.
		return nil, "the bead is " + cur.Status + " but still carries this ref's claim (its run label or assignee): a parked claim, released by its holder"
	}
	return &gcCandidate{ref: r, why: GCOrphan}, ""
}

// bdNoIssue is bd's --json error for an id with no bead (bd 1.3.x and the
// 2.0 envelope alike); bdx puts it in the error text.
const bdNoIssue = "no issues found matching the provided IDs"

// beadMissing reports whether a bd show error says the bead does not exist.
// Only bd's own wording counts: bd missing from PATH, or a database or remote
// "not found", must never pass for a missing bead and free its ref.
func beadMissing(err error) bool {
	return !errors.Is(err, bdx.ErrNotFound) && strings.Contains(err.Error(), bdNoIssue)
}

// gcOne deletes c's ref under the bead's lock, after reading the bead and this
// clone's holds again. It reports kept when the re-check found the ref is no
// longer garbage, with why in g.Detail.
func (e *Env) gcOne(ctx context.Context, c gcCandidate, g *GCRef, warnings *[]string) (gcResult, error) {
	id := c.ref.ID
	unlock, err := e.lockBead(ctx, "gc", id, warnings, 0) // a busy bead waits for the next gc
	if err != nil {
		g.Detail = lock.Scrub(err.Error())
		if lock.CodeOf(err) == lock.CodeContention {
			e.event("gc", id, "bead_lock", eventDeferred, "", nil)
		}
		return gcLost, err
	}
	defer unlock()
	if why := e.heldHere(id); why != "" {
		g.Detail = why
		return gcKept, nil
	}
	cur, err := e.show(ctx, id)
	switch {
	case err != nil && c.why == GCOrphan && beadMissing(err):
		// still missing, still an orphan
	case err != nil:
		g.Detail = "re-read: " + lock.Scrub(err.Error())
		return gcLost, err
	default:
		if again, keep := e.gcVerdict(cur, c.ref, c.why == GCOrphan); again == nil {
			if keep == "" {
				keep = "changed since gc read it: now " + cur.Status
				if cur.Status == "in_progress" {
					keep += ", claimed by this ref's run or actor"
				}
			}
			g.Detail = keep
			e.event("gc", id, "ledger_changed", eventDeferred, c.ref.SHA, nil)
			return gcKept, nil
		}
	}
	res, err := e.Lock.Delete(ctx, id, c.ref.SHA)
	if err != nil {
		g.Detail = "delete: " + lock.Scrub(err.Error())
		e.event("gc", id, "ref_delete", OutcomeUnreachable, c.ref.SHA, err)
		return gcLost, err
	}
	*warnings = append(*warnings, prefixed(id, res.Warnings)...)
	if res.Outcome != lock.Gone {
		g.Detail = "the ref changed since gc read it; run gc --dry-run again"
		e.event("gc", id, "ref_delete", OutcomeLost, c.ref.SHA, nil)
		return gcLost, nil
	}
	e.event("gc", id, "ref_delete", "deleted", c.ref.SHA, nil)
	return gcDeleted, nil
}

func prefixed(id string, ws []string) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, id+": "+w)
	}
	return out
}
