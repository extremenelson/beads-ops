package intent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// ReapOpts are the arguments of `reap`.
type ReapOpts struct {
	OlderThan time.Duration // zero means env.StaleAfter; shorter than it is refused
	DryRun    bool
	// Only, when set, limits reap to these bead ids, normally the ones a
	// preview listed under would_release. One that is no longer a stale claim
	// is listed under skipped; in a real run, one kept on evidence makes the
	// run negative too.
	Only []string
}

// ReapKept is a stale claim reap left alone, with what it found.
type ReapKept struct {
	ID       string   `json:"id"`
	Run      string   `json:"run"`
	Actor    string   `json:"actor"`
	Age      int      `json:"age_minutes"`
	Same     bool     `json:"same_machine"`
	Evidence []string `json:"evidence"`
}

// ReapDone is a stale claim reap released, would release, or could not.
type ReapDone struct {
	ID      string `json:"id"`
	Run     string `json:"run"`
	Actor   string `json:"actor"`
	Age     int    `json:"age_minutes"`
	Same    bool   `json:"same_machine"`
	Outcome string `json:"outcome,omitempty"`
	Detail  string `json:"detail,omitempty"`
	SavedTo string `json:"saved_to,omitempty"`
}

// ReapOut is the `reap` output.
type ReapOut struct {
	DryRun       bool       `json:"dry_run"`
	Released     []ReapDone `json:"released"`
	WouldRelease []ReapDone `json:"would_release"`
	Failed       []ReapDone `json:"failed"`
	Skipped      []ReapDone `json:"skipped"`
	Kept         []ReapKept `json:"kept"`
	Warnings     []string   `json:"warnings"`
	// namedKept counts ids --only named that a real run kept on evidence.
	namedKept int
	// pending counts releases whose ledger push did not land: their refs
	// stay until a sync from this checkout pushes them.
	pending int
}

// Negative reports a run that left a claim it meant to release.
func (o *ReapOut) Negative() bool {
	return o != nil && (len(o.Failed) > 0 || len(o.Skipped) > 0 || o.namedKept > 0 || o.pending > 0)
}

// ReapNotSeen is printed by every reap: a worktree on another machine, and
// uncommitted work in it, is invisible from here.
const ReapNotSeen = "worktrees on other machines, and other clones on this one, are not visible from here: a claim made there may hold uncommitted work"

// takeoverClock, recheckClock and releaseClock bound one takeover, the evidence
// read after it and its release, each on its own clock. reap starts no takeover with less than both left, so the pair
// ends inside the command's budget and a claim it took is given back.
const (
	takeoverClock = 90 * time.Second
	recheckClock  = 30 * time.Second // the evidence read between the two
	releaseClock  = 60 * time.Second
	reapReserve   = takeoverClock + recheckClock + releaseClock
)

// workLabels say a bead's work exists somewhere reap may not see: needs-land
// is how a loop marks work left in a worktree for a person to land, and that
// worktree may be on another machine.
var workLabels = []string{"needs-land"}

// mrURL matches a GitHub pull request or GitLab merge request URL in bead text.
var mrURL = regexp.MustCompile(`https?://[^\s)>\]"'\x00-\x1f\x7f]+/(?:pull|pulls|-/merge_requests|pull-requests)/[0-9]+`)

// Reap releases stale claims that hold no work anyone can see from here. The
// evidence of work is a needs-land label, a merge request URL on the bead, a
// hold of a live worktree of this clone, or a branch whose name carries the
// bead's id: on the lock remote or origin, local, or checked out (or named by
// the path of a detached worktree) in this clone. A claim with any evidence is
// kept and listed with what was found, for a person to decide.
//
// With DryRun false it pulls the ledger first, and for each claim lists the
// branches and reads the evidence again just before taking it over with claim
// --force (which re-checks under a lease that it is still stale), and once
// more after the takeover's pull. Work that appears then keeps the claim with
// this run, reported under failed, rather than release it. A claim is
// released with reason abandoned.
//
// If the ledger pull or the first branch listing fails, nothing is released;
// a later listing failure stops the run and lists the rest under skipped. A
// bead whose evidence cannot be read is kept. Orphan refs (a ref with no claim on the ledger) and
// landing refs are not stale claims and are left alone.
func Reap(ctx context.Context, env *Env, o ReapOpts) (*ReapOut, error) {
	out := &ReapOut{DryRun: o.DryRun, Released: []ReapDone{}, WouldRelease: []ReapDone{},
		Failed: []ReapDone{}, Skipped: []ReapDone{}, Kept: []ReapKept{}, Warnings: []string{ReapNotSeen}}
	if env.LockOff || env.Lock == nil {
		return out, fmt.Errorf("%w: reap needs the claim lock (lock.mode is none)", ErrPrecondition)
	}
	if o.OlderThan != 0 && o.OlderThan < env.staleAfter() {
		return out, fmt.Errorf("%w: --older-than %s is shorter than stale_minutes (%s), and claim --force would refuse those claims",
			ErrPrecondition, o.OlderThan, env.staleAfter())
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
		// The ledger must be the one claims write: one on another repository
		// would judge the claims and their evidence from the wrong beads.
		if err := env.checkRemote(ctx); err != nil {
			return out, err
		}
		if _, err := withLockRetry(ctx, env, "dolt", "pull"); err != nil {
			return out, fmt.Errorf("reap: ledger pull failed, so nothing was released: %w", err)
		}
	}
	st, err := Stale(ctx, env, StaleOpts{OlderThan: o.OlderThan, AllMachines: true})
	if err != nil {
		return out, err
	}
	out.Warnings = append(out.Warnings, st.Warnings...)
	if len(only) > 0 {
		var picked []StaleClaim
		for _, id := range only {
			if k := slices.IndexFunc(st.Stale, func(c StaleClaim) bool { return c.ID == id }); k >= 0 {
				picked = append(picked, st.Stale[k])
			} else {
				out.Skipped = append(out.Skipped, ReapDone{ID: id, Detail: "not a stale claim now; run reap --dry-run again"})
			}
		}
		st.Stale = picked
	}
	if len(st.Stale) == 0 {
		return out, nil
	}
	branches, err := env.workBranches(ctx)
	if err != nil {
		return out, fmt.Errorf("reap: cannot list branches, so nothing was released: %w", err)
	}
	var failures []error
	for k, c := range st.Stale {
		d := doneAs(c)
		if !o.DryRun && !timeFor(ctx) {
			for _, rest := range st.Stale[k:] {
				s := doneAs(rest)
				s.Detail = "not reached: too little of the command's time left; run reap again"
				out.Skipped = append(out.Skipped, s)
			}
			break
		}
		if !o.DryRun && k > 0 { // the first claim's listing is fresh
			if branches, err = env.workBranches(ctx); err != nil {
				why := "not reached: branches could not be listed again; run reap again"
				if ctx.Err() != nil {
					why = "not reached: too little of the command's time left; run reap again"
				} else {
					failures = append(failures, fmt.Errorf("reap: cannot list branches, so nothing more was released: %w", err))
				}
				for _, rest := range st.Stale[k:] {
					s := doneAs(rest)
					s.Detail = why
					out.Skipped = append(out.Skipped, s)
				}
				break
			}
		}
		ev := env.evidenceOrUnread(ctx, c.ID, branches, true)
		if c.Run == "" && !slices.Contains(only, c.ID) {
			ev = append(ev, "claimed outside bops (no run label or claim ref; its age is from the ledger only): name it with --only to release it")
		}
		if len(ev) > 0 {
			out.Kept = append(out.Kept, keptAs(c, ev))
			if !o.DryRun && slices.Contains(only, c.ID) {
				out.namedKept++
			}
			continue
		}
		if o.DryRun {
			out.WouldRelease = append(out.WouldRelease, d)
			continue
		}
		if !timeFor(ctx) { // the evidence read may have used the margin
			for _, rest := range st.Stale[k:] {
				s := doneAs(rest)
				s.Detail = "not reached: too little of the command's time left; run reap again"
				out.Skipped = append(out.Skipped, s)
			}
			break
		}
		if err := env.reapOne(ctx, c, &d, &out.Warnings); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", c.ID, err))
		}
		if d.Outcome == OutcomeReleased {
			if strings.Contains(d.Detail, reapPending) {
				out.pending++
			}
			out.Released = append(out.Released, d)
		} else {
			out.Failed = append(out.Failed, d)
		}
	}
	return out, errors.Join(failures...)
}

func keptAs(c StaleClaim, ev []string) ReapKept {
	return ReapKept{ID: c.ID, Run: c.Run, Actor: c.Actor, Age: c.AgeMinutes, Same: c.SameMachine, Evidence: ev}
}

func doneAs(c StaleClaim) ReapDone {
	return ReapDone{ID: c.ID, Run: c.Run, Actor: c.Actor, Age: c.AgeMinutes, Same: c.SameMachine}
}

// timeFor reports whether ctx leaves room for one more takeover and release.
func timeFor(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) >= reapReserve
}

// evidenceOrUnread is workEvidence with a read failure as evidence, so a bead
// that cannot be read is kept.
func (e *Env) evidenceOrUnread(ctx context.Context, id string, branches []workBranch, ownHold bool) []string {
	ev, err := e.workEvidence(ctx, id, branches, ownHold)
	if err != nil {
		return []string{"unread: " + lock.Scrub(err.Error())}
	}
	return ev
}

// reapOne takes c over and gives it back. claim --force refuses a claim that
// was refreshed since stale read it, so a holder that came back keeps it. The
// evidence is read once more after the takeover, whose pull may bring work
// another machine recorded; if any appears, this run keeps the claim and says
// so. The takeover and release run on their own clock, which Reap checked was
// there, so the command's deadline cannot cut them apart.
//
// All three steps run under one hold of the bead's lock, so no sync or other
// bops process in this clone acts on the claim between them. The release
// deletes the claim ref only once its ledger push has landed; if the push
// fails the ref stays, marked release pending, and the next sync (or command
// on the bead) from this clone pushes and then deletes it. So no machine sees
// the ref gone while the remote ledger still shows the old holder.
func (e *Env) reapOne(ctx context.Context, c StaleClaim, d *ReapDone, warnings *[]string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), takeoverClock)
	defer cancel()
	warn := func(ws []string) {
		for _, w := range ws {
			*warnings = append(*warnings, c.ID+": "+w)
		}
	}
	// held is the instruction for a claim this run may now hold; it is given
	// only when this run's state records the hold.
	held := func() string {
		if e.Run == nil {
			return ""
		}
		if _, ok := e.Run.Held[c.ID]; !ok { // this run's own journal, not a stranded hold
			return ""
		}
		return "; this run now holds " + c.ID + "; once decided, run `bops release " + c.ID + " --reason abandoned` from this checkout"
	}
	var lockWarnings []string
	unlock, err := e.holdBead(rctx, "reap", c.ID, &lockWarnings)
	warn(lockWarnings)
	if err != nil {
		d.Outcome, d.Detail = OutcomeError, "takeover: "+lock.Scrub(err.Error())
		return err
	}
	defer unlock()
	// Reap reads the evidence itself once the takeover is done, so it
	// takes over through claimLocked, which leaves the handoff unread.
	cl, err := claimLocked(rctx, e, c.ID, true, &ClaimOut{ID: c.ID, Run: e.Run.ID, Warnings: []string{}})
	if cl != nil {
		warn(cl.Warnings)
	}
	if err != nil {
		d.Outcome, d.Detail = OutcomeError, "takeover: "+lock.Scrub(err.Error())+held()
		return err
	}
	if cl.Outcome != OutcomeClaimed {
		why := cl.Reason
		if cl.Holder != nil {
			why = strings.TrimSpace(why + " held by " + orDash(cl.Holder.Actor) + " (run " + orDash(cl.Holder.Run) + ")")
		}
		d.Outcome, d.Detail = cl.Outcome, "takeover: "+why
		return nil
	}
	pending := cl.LedgerPending || cl.RefPending
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), recheckClock)
	defer ccancel()
	branches, err := e.workBranches(cctx)
	if err != nil {
		d.Outcome, d.Detail = OutcomeHeld, "taken over, but branches could not be listed again: "+lock.Scrub(err.Error())+held()
		return err
	}
	if ev := e.evidenceOrUnread(cctx, c.ID, branches, false); len(ev) > 0 {
		d.Outcome, d.Detail = OutcomeHeld, "taken over, then found work: "+strings.Join(ev, "; ")+held()
		return nil
	}
	note := fmt.Sprintf("Reaped: the claim by %s (run %s) was idle %d minutes, and no needs-land label, merge request, branch or worktree of this clone carried its work.\n\n"+
		"NEXT: pick it up again from the bead once its bops:hold cooling-off ends; anything left in a worktree on the holder's machine is not on the remote.",
		orDash(c.Actor), orDash(c.Run), c.AgeMinutes)
	relctx, relcancel := context.WithTimeout(context.WithoutCancel(ctx), releaseClock)
	defer relcancel()
	rel, err := releaseKeepingRef(relctx, e, c.ID, "abandoned", note)
	if rel != nil {
		warn(rel.Warnings)
		d.Outcome, d.Detail, d.SavedTo = rel.Outcome, rel.Detail, rel.SavedTo
		if rel.Outcome == OutcomeReleased {
			// The release's own push carries the takeover's commit too, so
			// only the release decides whether anything still waits.
			pending = rel.LedgerPending || rel.RefPending
		} else {
			pending = pending || rel.LedgerPending || rel.RefPending
		}
	}
	if pending {
		d.Detail = strings.TrimSpace(d.Detail + " " + reapPending)
	}
	if err != nil || d.Outcome != OutcomeReleased {
		if d.Outcome == "" {
			d.Outcome = OutcomeError
		}
		if err != nil {
			d.Detail = strings.TrimSpace(d.Detail + " release: " + lock.Scrub(err.Error()))
		}
		d.Detail = strings.TrimPrefix(strings.TrimSpace(d.Detail+held()), "; ")
	}
	return err
}

// releaseKeepingRef is ReleaseWithText under a bead lock the caller holds,
// keeping the claim ref until the release's ledger push lands.
func releaseKeepingRef(ctx context.Context, e *Env, id, reason, text string) (*ReleaseOut, error) {
	out, err := newRelease(e, id, reason, text, "")
	if err != nil {
		return nil, err
	}
	out, err = releaseLocked(ctx, e, out, text, true)
	out.keepText(text)
	return out, err
}

// reapPending marks a release, or takeover, whose ledger or ref write waits on
// a sync from this checkout.
const reapPending = "(pending: run bops sync from this checkout)"

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// workBranch is a name that may carry a bead id, and where it was seen.
type workBranch struct{ name, where string }

// workBranches lists branch names on the lock remote and on origin, this
// clone's local branches, and for each worktree of this clone its branch and
// its directory name (a detached worktree has only the latter).
func (e *Env) workBranches(ctx context.Context) ([]workBranch, error) {
	remotes := []string{e.Lock.RemoteName()}
	if remotes[0] != "origin" {
		res, err := e.Lock.Git.Run(ctx, lock.Cmd{Args: []string{"remote"}})
		if err != nil {
			return nil, fmt.Errorf("git remote: %w", err)
		}
		if slices.Contains(strings.Fields(res.Stdout), "origin") {
			remotes = append(remotes, "origin")
		}
	}
	var out []workBranch
	for _, r := range remotes {
		res, err := e.Lock.Git.Run(ctx, lock.Cmd{Args: []string{"ls-remote", "--heads", r}})
		if err != nil {
			return nil, fmt.Errorf("git ls-remote --heads %s: %w", r, err)
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				out = append(out, workBranch{strings.TrimPrefix(f[1], "refs/heads/"), "remote " + r})
			}
		}
	}
	res, err := e.Lock.Git.Run(ctx, lock.Cmd{Args: []string{"for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes"}})
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %w", err)
	}
	for _, ref := range strings.Fields(res.Stdout) {
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			out = append(out, workBranch{name, "local"})
		} else if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
			// Other remotes, such as a fork, as of their last fetch.
			if r, name, ok := strings.Cut(rest, "/"); ok && name != "HEAD" {
				out = append(out, workBranch{name, "remote " + r + " (last fetch)"})
			}
		}
	}
	res, err = e.Lock.Git.Run(ctx, lock.Cmd{Args: []string{"worktree", "list", "--porcelain"}})
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	path := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
			out = append(out, workBranch{filepath.Base(path), "worktree directory " + path})
		case strings.HasPrefix(line, "branch "):
			out = append(out, workBranch{strings.TrimPrefix(line, "branch refs/heads/"), "worktree " + path})
		}
	}
	return out, nil
}

// workEvidence lists what suggests id's work exists: a work label, a merge
// request URL on the bead (landed metadata, notes or comments), a hold of a
// live worktree of this clone, or a name in branches that carries the id.
// ownHold counts this run's own hold too; after reap's own takeover it must
// not, since that hold is the takeover.
func (e *Env) workEvidence(ctx context.Context, id string, branches []workBranch, ownHold bool) ([]string, error) {
	cur, err := e.show(ctx, id)
	if err != nil {
		return nil, err
	}
	var ev []string
	for _, l := range workLabels {
		if cur.HasLabel(l) {
			ev = append(ev, "label "+l)
		}
	}
	landed, err := cur.Landed()
	if err != nil {
		return nil, err
	}
	for _, l := range landed {
		ev = append(ev, "mr "+l.Ref)
	}
	comments, err := e.comments(ctx, id)
	if err != nil {
		return nil, err
	}
	texts := []string{cur.Notes, cur.Description, cur.AcceptanceCriteria}
	for _, c := range comments {
		texts = append(texts, c.Text)
	}
	for _, t := range texts {
		for _, u := range mrURL.FindAllString(t, -1) {
			ev = append(ev, "mr "+u)
		}
	}
	if e.Store != nil {
		for _, h := range e.Store.Holds() {
			if h.ID == id && !h.Stranded && (ownHold || e.Run == nil || h.Run != e.Run.ID) {

				ev = append(ev, "hold of run "+h.Run+" in live worktree "+h.Worktree)
			}
		}
	}
	for _, b := range branches {
		if BranchNamesBead(b.name, id) {
			ev = append(ev, "branch "+b.name+" ("+b.where+")")
		}
	}
	slices.Sort(ev)
	return slices.Compact(ev), nil
}

// BranchNamesBead reports whether a branch or directory name carries a bead
// id, or the id of one of its parents (a child's work may sit on its epic's
// branch): the whole id, or its part after the prefix with or without dots,
// as a token bounded by the ends or one of / - _ . It errs towards a match: a
// false match keeps a claim, a missed one could release work.
func BranchNamesBead(branch, id string) bool {
	b := strings.ToLower(branch)
	for i := strings.ToLower(id); i != ""; {
		if tokenIn(b, i) {
			return true
		}
		if k := strings.LastIndex(i, "-"); k >= 0 && k < len(i)-1 {
			suffix := i[k+1:]
			if tokenIn(b, suffix) || tokenIn(b, strings.ReplaceAll(suffix, ".", "")) {
				return true
			}
		}
		dot := strings.LastIndex(i, ".")
		if dot <= strings.LastIndex(i, "-") {
			break
		}
		i = i[:dot] // the parent
	}
	return false
}

// tokenIn reports whether tok occurs in s bounded by the start, the end or
// one of / - _ . on each side.
func tokenIn(s, tok string) bool {
	const bounds = "/-_."
	for from := 0; ; {
		k := strings.Index(s[from:], tok)
		if k < 0 {
			return false
		}
		k += from
		end := k + len(tok)
		if (k == 0 || strings.ContainsRune(bounds, rune(s[k-1]))) &&
			(end == len(s) || strings.ContainsRune(bounds, rune(s[end]))) {
			return true
		}
		from = k + 1
	}
}
