package intent

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// Claim states of `status`.
const (
	StatusLive    = "live"    // held and touched within OlderThan
	StatusStale   = "stale"   // held and idle longer than OlderThan (what stale lists)
	StatusLanding = "landing" // done, waiting on its merge request
	StatusOrphan  = "orphan"  // a held ref older than OlderThan with no claim on the ledger
	StatusUnknown = "unknown" // staleness could not be judged: an unreadable ref, or unread comments
)

// StatusOpts are the arguments of `status`.
type StatusOpts struct {
	OlderThan time.Duration // idle longer than this is stale; zero means env.StaleAfter
	Mine      bool          // only claims made on this machine
	Stale     bool          // only stale, orphan and unknown claims
}

// StatusClaim is one claim on the board.
type StatusClaim struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	State   string `json:"state"`
	Actor   string `json:"actor"`
	Machine string `json:"machine,omitempty"`
	Run     string `json:"run"`
	// AgeMinutes is since the run's bops:claimed comment, else the bead's
	// started_at; nil when neither is known.
	AgeMinutes *int `json:"age_minutes"`
	// IdleMinutes is since LastEventAt, the age stale measures; both are nil
	// when unknown (an unreadable or future at, or a landing ref with none).
	IdleMinutes *int       `json:"idle_minutes"`
	LastEventAt *time.Time `json:"last_event_at"`
	// LastNoteMinutes is since the newest bops note by Run (by any run when
	// Run is unknown); nil when there is none.
	LastNoteMinutes *int   `json:"last_note_minutes"`
	Branch          string `json:"branch,omitempty"`
	MR              string `json:"mr,omitempty"`
	SameMachine     bool   `json:"same_machine"`
	// Source is where the claim was seen: "ref", "ledger" (no ref) or
	// "ref+ledger".
	Source string `json:"source"`
	// Touches is the paths the bead declares with touches: labels; empty for
	// a ref's bead outside the in-progress list.
	Touches []string `json:"touches,omitempty"`
}

// StatusOut is the `status` output.
type StatusOut struct {
	Claims []StatusClaim `json:"claims"`
	// LedgerConflict is this clone's last ledger pull that stopped on a
	// merge conflict needing a person, while no later pull has succeeded;
	// every pull and push from the clone fails until it is resolved.
	LedgerConflict *LedgerConflictReport `json:"ledger_conflict,omitempty"`
	Warnings       []string              `json:"warnings"`
}

// Status lists every claim, from the claim refs and the ledger joined, with
// its holder, machine, run, ages, branch and merge request. It never writes.
//
// A bead is on the board when it has a claim ref or is in progress on the
// ledger with an assignee, a run: label or bops:landing. The ref, when there
// is one, decides the state: landing for a landing ref; unknown for a ref
// whose state is unreadable (never stale, as for stale and claim --force);
// for a held ref, unknown when its at is unreadable or in the future (never
// stale either), else stale when its at is older than OlderThan and orphan when
// its bead has no claim by the ref's run or actor (a younger one is live,
// usually a claim whose ledger write is pending). A bead with no ref is
// landing with bops:landing, else aged from the ledger as stale ages it, and
// unknown when that age is past OlderThan but its comments could not be read.
// A ref's bead outside the in-progress list (closed, blocked, deferred) is
// judged from the ref alone. Unlike stale, a mismatched stale ref is one
// orphan row, not also a stale one, and same_machine also counts the ref's
// machine. Branch and MR are inferred as claim --force's previous infers
// them; the forge is not asked, and branches are not listed when the claim
// ref fetch failed. A failed read beyond the refs and the bead list leaves
// fields empty and adds a warning. A ledger pull this clone recorded as
// stuck on a merge conflict is reported in ledger_conflict and a warning.
func Status(ctx context.Context, env *Env, o StatusOpts) (*StatusOut, error) {
	if o.OlderThan == 0 {
		o.OlderThan = env.staleAfter()
	}
	out := &StatusOut{Claims: []StatusClaim{}, Warnings: []string{}}
	out.LedgerConflict = env.ledgerConflictReport(&out.Warnings) // read from the record: status pulls nothing
	refs, issues, fetched, err := env.claimSnapshot(ctx, &out.Warnings)
	if err != nil {
		return nil, err
	}
	now := env.now()
	cutoff := now.Add(-o.OlderThan)
	mine := env.clonesRuns()
	machine := ""
	if env.Run != nil {
		machine = env.Run.Machine
	}

	var branches []workBranch
	switch {
	case env.LockOff || env.Lock == nil:
	case !fetched: // the remotes will not answer ls-remote either; do not spend the budget waiting
		out.Warnings = append(out.Warnings, "branches not read: the lock remote did not answer the fetch")
	default:
		if branches, err = env.workBranches(ctx); err != nil {
			out.Warnings = append(out.Warnings, "branches not read: "+lock.Scrub(err.Error()))
		}
	}
	minutes := func(at time.Time) *int {
		if at.IsZero() || at.After(now) {
			return nil
		}
		m := int(now.Sub(at).Minutes())
		return &m
	}

	// byID is every in-progress bead, for a ref row's title and notes; only
	// one with a claim on the ledger is on the board without a ref.
	byID := map[string]*ledger.Issue{}
	ids := map[string]bool{}
	for k := range issues {
		i := &issues[k]
		byID[i.ID] = i
		if i.Assignee != "" || len(i.LabelsWithPrefix("run:")) > 0 || i.HasLabel("bops:landing") {
			ids[i.ID] = true
		}
	}
	for id, r := range refs {
		if r.State == lock.StateHeld || r.State == lock.StateLanding {
			ids[id] = true
		}
	}

	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted) // warnings name beads in a stable order

	commentsFailed := 0
	for _, id := range sorted {
		i := byID[id]
		r, refPresent := refs[id]
		hasRef := refPresent && (r.State == lock.StateHeld || r.State == lock.StateLanding)
		c := StatusClaim{ID: id, Source: "ledger"}
		if i != nil {
			c.Title, c.Actor, c.Touches = i.Title, i.Assignee, touches(i)
			if runs := i.LabelsWithPrefix("run:"); len(runs) > 0 {
				c.Run = runs[0]
			}
		}
		if refPresent {
			c.Source = "ref+ledger"
			if i == nil {
				c.Source = "ref"
			}
		}
		if hasRef {
			c.Run, c.Actor, c.Machine = r.Run, r.Actor, r.Machine
		}

		// The state, judged as stale judges it. A ref, when there is one,
		// decides; the bops:landing label only for a bead with no ref.
		var lastEvent time.Time // zero while unknown
		judge := func(comments []bdComment, commentsOK bool) string {
			lastEvent = time.Time{}
			switch {
			case hasRef && r.State == lock.StateLanding:
				lastEvent = r.At
				return StatusLanding
			case !refPresent && i != nil && i.HasLabel("bops:landing"):
				lastEvent = ledgerActivity(i, comments)
				return StatusLanding
			case refPresent && !hasRef:
				return StatusUnknown // an unreadable ref is never stale, and claim --force refuses it
			case hasRef && (r.At.IsZero() || r.Anomalous(now)):
				return StatusUnknown // an unreadable or future at is never stale, and claim --force refuses it
			case hasRef:
				lastEvent = r.At
				stale := !r.At.After(cutoff)
				if i == nil || !(i.HasLabel("run:"+r.Run) || i.Assignee == r.Actor) {
					if stale {
						return StatusOrphan
					}
					return StatusLive // probably a claim whose ledger write is pending
				}
				if stale {
					return StatusStale
				}
				return StatusLive
			}
			lastEvent = ledgerActivity(i, comments)
			switch {
			case lastEvent.After(cutoff):
				return StatusLive // a comment can only make it fresher
			case !commentsOK:
				return StatusUnknown // a bops: comment we could not read may be newer
			}
			return StatusStale
		}
		// --stale needs no comments to drop a claim that is fresh without them.
		if o.Stale {
			if st := judge(nil, false); st == StatusLive || st == StatusLanding { // unknown stays: it may be stale
				continue
			}
		}
		var comments []bdComment
		commentsOK := true
		if cs, err := env.comments(ctx, id); err != nil {
			commentsOK = false
			commentsFailed++
			if commentsFailed == 1 {
				out.Warnings = append(out.Warnings, "comments not read for "+id+": "+lock.Scrub(err.Error()))
			}
		} else {
			comments = cs
		}
		c.State = judge(comments, commentsOK)
		if !lastEvent.IsZero() {
			c.LastEventAt = &lastEvent
		}
		c.IdleMinutes = minutes(lastEvent)

		claimedMachine, claimedAt := claimedBy(comments, c.Run)
		if c.Machine == "" {
			c.Machine = claimedMachine
		}
		switch {
		case !claimedAt.IsZero():
			c.AgeMinutes = minutes(claimedAt)
		case i != nil && i.StartedAt != nil:
			c.AgeMinutes = minutes(*i.StartedAt)
		}
		c.LastNoteMinutes = minutes(lastNoteAt(comments, c.Run))
		c.SameMachine = mine[c.Run] || (machine != "" && c.Machine == machine) ||
			(c.Run != "" && claimedHere(comments, c.Run, machine))

		if o.Mine && !c.SameMachine {
			continue
		}
		if o.Stale && c.State != StatusStale && c.State != StatusOrphan && c.State != StatusUnknown {
			continue
		}
		var landed []ledger.Landed
		notes := ""
		if i != nil {
			notes = i.Notes
			if landed, err = i.Landed(); err != nil {
				out.Warnings = append(out.Warnings, "landed metadata not read for "+id+": "+lock.Scrub(err.Error()))
			}
		}
		c.MR = newestMR(landed, notes, comments)
		c.Branch = pickBranch(branches, id, "")
		out.Claims = append(out.Claims, c)
	}
	if commentsFailed > 1 {
		out.Warnings = append(out.Warnings, "comments not read for "+strconv.Itoa(commentsFailed-1)+" more beads: their age, last note and machine may be missing")
	}
	sort.Slice(out.Claims, func(a, b int) bool { return out.Claims[a].ID < out.Claims[b].ID })
	return out, nil
}

// claimedBy is the machine and time of run's newest bops:claimed comment.
func claimedBy(comments []bdComment, run string) (machine string, at time.Time) {
	if run == "" {
		return "", time.Time{}
	}
	prefix := "bops:claimed run=" + run + " "
	for _, c := range comments {
		if !strings.HasPrefix(c.Text, prefix) {
			continue
		}
		if !at.IsZero() && c.CreatedAt.Before(at) {
			continue
		}
		at, machine = c.CreatedAt, ""
		for _, f := range strings.Fields(strings.SplitN(c.Text, "\n", 2)[0]) {
			if v, ok := strings.CutPrefix(f, "machine="); ok {
				machine = v
			}
		}
	}
	return machine, at
}

// lastNoteAt is when the newest bops note by run was written, or by any run
// when run is empty.
func lastNoteAt(comments []bdComment, run string) time.Time {
	var at time.Time
	for _, c := range comments {
		if strings.HasPrefix(c.Text, notePrefix) && (run == "" || noteRun(c.Text) == run) && c.CreatedAt.After(at) {
			at = c.CreatedAt
		}
	}
	return at
}
