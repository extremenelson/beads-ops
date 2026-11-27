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

	"github.com/extremenelson/beads-ops/bops/internal/config"
	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
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
	// Forge, when it is a forge.Searcher, is asked for open or merged merge
	// requests naming each claim that has no other evidence, and again after
	// its takeover. Nil, or a forge that cannot search (none), skips that and
	// says so in a warning.
	Forge forge.Forge
	// ForgeClock bounds one forge search; zero means 20 s.
	ForgeClock time.Duration
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

// takeoverClock, recheckClock, forgeClock and releaseClock bound one
// takeover, the evidence read and forge search after it, and its release,
// each on its own clock. reap starts no takeover with less than all of them
// left, so they end inside the command's budget and a claim it took is given
// back.
const (
	takeoverClock = 90 * time.Second
	recheckClock  = 30 * time.Second // the evidence read between the two
	// forgeClock bounds one forge search. One that does not answer in time
	// keeps the claim.
	forgeClock   = 20 * time.Second
	releaseClock = 60 * time.Second
	reapReserve  = takeoverClock + recheckClock + forgeClock + releaseClock
)

// ReapNoForge is warned when reap has no forge to search for merge requests.
const ReapNoForge = "no forge to search (forge none, or not inferred from the lock remote): merge requests from forks, or merged with their branch deleted, were not looked for"

// ReapForgeUnanswered is warned once when a forge search fails: every claim
// with no other evidence is then kept.
const ReapForgeUnanswered = "the forge search did not answer, so claims with no other evidence were kept; fix the forge CLI (gh or glab) or its login, or pass --forge none to release without searching"

// workLabels say a bead's work exists somewhere reap may not see: needs-land
// is how a loop marks work left in a worktree for a person to land, and that
// worktree may be on another machine.
var workLabels = []string{"needs-land"}

// mrURL matches a GitHub pull request or GitLab merge request URL in bead text.
var mrURL = regexp.MustCompile(`https?://[^\s)>\]"'\x00-\x1f\x7f]+/(?:pull|pulls|-/merge_requests|pull-requests)/[0-9]+`)

// Reap releases stale claims that hold no work anyone can see from here. The
// evidence of work is a needs-land label, a merge request URL on the bead, a
// hold of a live worktree of this clone, a branch whose name carries the
// bead's id: on the lock remote or origin, local, or checked out (or named by
// the path of a detached worktree) in this clone, or an open or merged merge
// request on the forge whose title or branch carries the id. A forge search
// that fails, times out or may have missed some is evidence too. A claim with
// any evidence is kept and listed with what was found, for a person to decide.
//
// The first evidence read of each claim reuses the issue and comments Stale
// decoded. With DryRun false it pulls the ledger first, and for each claim
// lists the branches again, and reads the bead again just before taking it
// over with claim --force (which re-checks under a lease that it is still
// stale), and once more after the takeover's pull. Work that appears then
// keeps the claim with this run, reported under failed, rather than release
// it. A claim is released with reason abandoned.
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
	fs := forgeSearch{clock: o.ForgeClock}
	fs.s, _ = o.Forge.(forge.Searcher)
	if fs.s == nil {
		out.Warnings = append(out.Warnings, ReapNoForge)
	}
	defer func() {
		if fs.unanswered != nil {
			out.Warnings = append(out.Warnings, ReapForgeUnanswered)
		}
	}()
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
		// The issue and comments Stale decoded serve here. A real run
		// reads a claim they show no work for afresh, since Stale's
		// snapshot may be minutes old by now, before the forge is asked
		// and the claim taken over.
		known := c
		if !o.DryRun && known.comments == nil {
			// Stale read none; the fresh read below reads them if the rest
			// shows no work, so reading them here too would double it.
			known.comments = []bdComment{}
		}
		ev := env.evidenceOrUnread(ctx, c.ID, &known, branches, true)
		if c.Run == "" && !slices.Contains(only, c.ID) {
			ev = append(ev, "claimed outside bops (no run label or claim ref; its age is from the ledger only): name it with --only to release it")
		}
		if len(ev) == 0 && !o.DryRun {
			ev = env.evidenceOrUnread(ctx, c.ID, nil, branches, true)
		}
		if len(ev) == 0 && o.DryRun && !fs.timeFor(ctx) {
			for _, rest := range st.Stale[k:] {
				s := doneAs(rest)
				s.Detail = "not reached: too little of the command's time left for a forge search; run reap again"
				out.Skipped = append(out.Skipped, s)
			}
			break
		}
		if len(ev) == 0 { // one forge call, only for a claim it may save
			ev = fs.evidence(ctx, c.ID)
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
		if !timeFor(ctx) { // the evidence reads may have used the margin
			for _, rest := range st.Stale[k:] {
				s := doneAs(rest)
				s.Detail = "not reached: too little of the command's time left; run reap again"
				out.Skipped = append(out.Skipped, s)
			}
			break
		}
		if err := env.reapOne(ctx, c, &d, &out.Warnings, &fs); err != nil {
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

// forgeSearch is reap's forge search: s, nil when there is no forge to
// search, and whether any search went unanswered.
type forgeSearch struct {
	s     forge.Searcher
	clock time.Duration // zero means forgeClock; never more
	// unanswered is the first search that failed or timed out; later ones
	// are not made, so a dead forge costs one clock, not one per claim.
	unanswered error
}

func (f *forgeSearch) clockOr() time.Duration {
	if f.clock <= 0 || f.clock > forgeClock { // the takeover reserve counts forgeClock
		return forgeClock
	}
	return f.clock
}

// timeFor reports whether a preview has time for one more forge search. One
// that has not lists the rest under skipped, rather than keep them on
// searches the deadline cut short. With no forge there is nothing to wait on.
func (f *forgeSearch) timeFor(ctx context.Context) bool {
	if f.s == nil || f.unanswered != nil { // no search will be made
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) >= f.clockOr()
}

// evidence asks the forge for merge requests naming id and lists the ones
// not closed unmerged whose title or source branch carries it. The forge's
// search matches titles and descriptions, so one that names id only in its
// branch is not found. A search that fails, times out, or hit its result
// limit without such a match is evidence too: an unanswered forge keeps the
// claim. With no forge it finds nothing.
func (f *forgeSearch) evidence(ctx context.Context, id string) []string {
	if f.s == nil {
		return nil
	}
	if f.unanswered != nil {
		return []string{"forge unanswered, so kept: an earlier search failed: " + lock.Scrub(f.unanswered.Error())}
	}
	fctx, cancel := context.WithTimeout(ctx, f.clockOr())
	defer cancel()
	mrs, err := f.s.SearchMRs(fctx, id)
	var ev []string
	for _, m := range mrs {
		// Closed unmerged is the one state that is not work; one this code
		// does not know keeps the claim.
		if m.State == "closed" {
			continue
		}
		if !BranchNamesBead(m.Branch, id) && !titleNamesBead(m.Title, id) {
			continue
		}
		u := mrURL.FindString(m.URL)
		if u == "" {
			u = "(no usable URL)"
		}
		ev = append(ev, "mr "+u+" ("+m.State+", found on the forge)")
	}
	if len(ev) == 0 && err != nil {
		if !errors.Is(err, forge.ErrIncomplete) { // a full page is an answer, just not a complete one
			f.unanswered = err
		}
		return []string{"forge unanswered, so kept: " + lock.Scrub(err.Error())}
	}
	slices.Sort(ev)
	return slices.Compact(ev)
}

// titleNamesBead reports whether a merge request title carries id, or the id
// of one of its parents, whole and bounded by the ends or by a character that
// is not a letter or digit. Unlike a branch, a title's bare suffix ("260") is
// not enough: prose is full of numbers.
func titleNamesBead(title, id string) bool {
	t := strings.ToLower(title)
	for i := strings.ToLower(id); i != ""; {
		for from := 0; ; {
			k := strings.Index(t[from:], i)
			if k < 0 {
				break
			}
			k += from
			if !alnumAt(t, k-1) && !alnumAt(t, k+len(i)) {
				return true
			}
			from = k + 1
		}
		dot := strings.LastIndex(i, ".")
		if dot <= strings.LastIndex(i, "-") {
			break
		}
		i = i[:dot] // the parent
	}
	return false
}

// alnumAt reports whether s has an ASCII letter or digit at k.
func alnumAt(s string, k int) bool {
	if k < 0 || k >= len(s) {
		return false
	}
	c := s[k]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// evidenceOrUnread is workEvidence with a read failure as evidence, so a bead
// that cannot be read is kept. known, when set, supplies the issue and the
// comments Stale already decoded; what it lacks is read fresh.
func (e *Env) evidenceOrUnread(ctx context.Context, id string, known *StaleClaim, branches []workBranch, ownHold bool) []string {
	var cur *ledger.Issue
	var comments []bdComment
	if known != nil {
		cur, comments = known.issue, known.comments
	}
	ev, err := e.workEvidence(ctx, id, cur, comments, branches, ownHold)
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
func (e *Env) reapOne(ctx context.Context, c StaleClaim, d *ReapDone, warnings *[]string, fs *forgeSearch) error {
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
	ev := e.evidenceOrUnread(cctx, c.ID, nil, branches, false)
	if len(ev) == 0 {
		// Under the bead lock, on its own clock: a merge request opened
		// since the first search keeps the claim too.
		ev = fs.evidence(context.WithoutCancel(ctx), c.ID)
	}
	if len(ev) > 0 {
		d.Outcome, d.Detail = OutcomeHeld, "taken over, then found work: "+strings.Join(ev, "; ")+held()
		return nil
	}
	searched := ""
	if fs.s != nil {
		searched = ", nor did the forge find an open or merged merge request naming it"
	}
	note := fmt.Sprintf("Reaped: the claim by %s (run %s) was idle %d minutes, and no needs-land label, merge request, branch or worktree of this clone carried its work%s.\n\n"+
		"NEXT: pick it up again from the bead once its bops:hold cooling-off ends; anything left in a worktree on the holder's machine is not on the remote.",
		orDash(c.Actor), orDash(c.Run), c.AgeMinutes, searched)
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
// not, since that hold is the takeover. A nil cur or comments is read from
// bd; a non-nil one is used as given.
func (e *Env) workEvidence(ctx context.Context, id string, cur *ledger.Issue, comments []bdComment, branches []workBranch, ownHold bool) ([]string, error) {
	var err error
	if cur == nil {
		if cur, err = e.show(ctx, id); err != nil {
			return nil, err
		}
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
	if comments == nil {
		if comments, err = e.comments(ctx, id); err != nil {
			return nil, err
		}
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
// one of config.IDBounds on each side. branch_pattern must carry <id> so
// bounded, so a branch it names is found here.
func tokenIn(s, tok string) bool {
	for from := 0; ; {
		k := strings.Index(s[from:], tok)
		if k < 0 {
			return false
		}
		k += from
		if config.Bounded(s, k, k+len(tok)) {
			return true
		}
		from = k + 1
	}
}
