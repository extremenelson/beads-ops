package intent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// StaleOpts are the arguments of `stale`.
type StaleOpts struct {
	OlderThan   time.Duration // idle longer than this is stale; zero means 2h
	AllMachines bool          // false hides claims made on other machines
}

// StaleClaim is a claim nobody has touched for longer than OlderThan.
type StaleClaim struct {
	ID          string    `json:"id"`
	Run         string    `json:"run"`
	Actor       string    `json:"actor"`
	LastEventAt time.Time `json:"last_event_at"`
	AgeMinutes  int       `json:"age_minutes"`
	SameMachine bool      `json:"same_machine"`
	// Source says where the age came from: "ref" (the claim ref's at, which
	// note refreshes) or "ledger" (a claim with no ref: updated_at,
	// heartbeat_at or the newest bops: comment).
	Source string `json:"source"`
}

// Orphan is a held claim ref with no matching claim on the ledger.
type Orphan struct {
	ID          string    `json:"id"`
	Run         string    `json:"run"`
	Actor       string    `json:"actor"`
	At          time.Time `json:"at"`
	AgeMinutes  int       `json:"age_minutes"`
	SameMachine bool      `json:"same_machine"`
}

// StaleOut is the `stale` output.
type StaleOut struct {
	Stale    []StaleClaim `json:"stale"`
	Orphans  []Orphan     `json:"orphans"`
	Warnings []string     `json:"warnings"`
}

// Stale lists claims idle for longer than o.OlderThan, and held refs whose
// bead shows no claim on the ledger. It reads the live refs when it can and
// the copy from the last sync when it cannot, and it never writes.
//
// Age comes from the claim ref's `at` when there is a ref, because that is
// what claim --force measures and note refreshes. A claim with no ref (raw
// bd, or made before the lock) is aged from the latest of updated_at,
// heartbeat_at and its newest bops: comment. A landing ref is waiting on a
// PR and is neither stale nor an orphan. A ref whose `at` is unreadable or in
// the future is never stale, matching claim --force.
func Stale(ctx context.Context, env *Env, o StaleOpts) (*StaleOut, error) {
	if o.OlderThan == 0 {
		o.OlderThan = 2 * time.Hour
	}
	out := &StaleOut{Stale: []StaleClaim{}, Orphans: []Orphan{}, Warnings: []string{}}
	refs := map[string]lock.Ref{}
	if env.Lock != nil {
		if err := env.Lock.Fetch(ctx); err != nil {
			out.Warnings = append(out.Warnings, "claim refs are as of the last sync: "+lock.Scrub(err.Error()))
		}
		seen, err := env.Lock.Seen(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range seen {
			refs[r.ID] = r
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

	now := env.now()
	mine := env.clonesRuns()
	cutoff := now.Add(-o.OlderThan)
	claimed := map[string]*ledger.Issue{}

	for k := range issues {
		i := &issues[k]
		runs := i.LabelsWithPrefix("run:")
		if i.Assignee == "" && len(runs) == 0 {
			continue // no claim on the ledger; a held ref here is an orphan, below
		}
		claimed[i.ID] = i
		c := StaleClaim{ID: i.ID, Actor: i.Assignee}
		if len(runs) > 0 {
			c.Run = runs[0]
		}
		var comments []bdComment
		if r, ok := refs[i.ID]; ok {
			if r.State != lock.StateHeld || r.At.IsZero() {
				continue // a future at is newer than the cutoff, so never stale either
			}
			c.Run, c.Actor, c.LastEventAt, c.Source = r.Run, r.Actor, r.At, "ref"
		} else {
			c.LastEventAt, c.Source = i.UpdatedAt, "ledger"
			if i.HeartbeatAt != nil && i.HeartbeatAt.After(c.LastEventAt) {
				c.LastEventAt = *i.HeartbeatAt
			}
			if c.LastEventAt.After(cutoff) {
				continue // already fresh; a comment can only make it fresher
			}
			if comments, err = env.comments(ctx, i.ID); err != nil {
				return nil, err
			}
			for _, cm := range comments {
				if strings.HasPrefix(cm.Text, "bops:") && cm.CreatedAt.After(c.LastEventAt) {
					c.LastEventAt = cm.CreatedAt
				}
			}
		}
		if c.LastEventAt.After(cutoff) {
			continue
		}
		c.AgeMinutes = int(now.Sub(c.LastEventAt).Minutes())
		c.SameMachine = mine[c.Run]
		if !c.SameMachine && c.Run != "" {
			if comments == nil {
				if comments, err = env.comments(ctx, i.ID); err != nil {
					return nil, err
				}
			}
			c.SameMachine = claimedHere(comments, c.Run, env.Run.Machine)
		}
		if c.SameMachine || o.AllMachines {
			out.Stale = append(out.Stale, c)
		}
	}

	for _, r := range refs {
		if r.State != lock.StateHeld || r.At.IsZero() || r.At.After(cutoff) {
			continue // a young orphan is usually a claim whose ledger write is still pending
		}
		if i := claimed[r.ID]; i != nil && (i.HasLabel("run:"+r.Run) || i.Assignee == r.Actor) {
			continue
		}
		orphan := Orphan{ID: r.ID, Run: r.Run, Actor: r.Actor, At: r.At,
			AgeMinutes: int(now.Sub(r.At).Minutes()), SameMachine: mine[r.Run]}
		if orphan.SameMachine || o.AllMachines {
			out.Orphans = append(out.Orphans, orphan)
		}
	}
	sort.Slice(out.Stale, func(a, b int) bool { return out.Stale[a].ID < out.Stale[b].ID })
	sort.Slice(out.Orphans, func(a, b int) bool { return out.Orphans[a].ID < out.Orphans[b].ID })
	return out, nil
}

type bdComment struct {
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

func (e *Env) comments(ctx context.Context, id string) ([]bdComment, error) {
	raw, err := e.BD.Run(ctx, "comments", id, "--json")
	if err != nil {
		return nil, err
	}
	out := []bdComment{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// clonesRuns is every run id in this clone's state, including runs of
// worktrees that are gone but still hold something.
func (e *Env) clonesRuns() map[string]bool {
	m := map[string]bool{}
	if e.Run != nil {
		m[e.Run.ID] = true
	}
	if e.Store != nil {
		for _, h := range e.Store.Holds() {
			m[h.Run] = true
		}
	}
	return m
}

// claimedHere reports whether run's bops:claimed comment names this
// machine. It covers runs whose worktree, and so whose state, is gone.
func claimedHere(comments []bdComment, run, machine string) bool {
	if machine == "" {
		return false
	}
	prefix := "bops:claimed run=" + run + " "
	for _, c := range comments {
		if !strings.HasPrefix(c.Text, prefix) {
			continue
		}
		for _, f := range strings.Fields(strings.SplitN(c.Text, "\n", 2)[0]) {
			if f == "machine="+machine {
				return true
			}
		}
	}
	return false
}
