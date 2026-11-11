package intent

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SessionStartOut is the `session-start` output: one compact document for a
// SessionStart hook. Everything but Prime stays small (under 1 kB on a
// 100-bead ledger); Prime is bd's own minimal text, capped.
type SessionStartOut struct {
	// Mine is what this worktree's run holds: resume these before new work.
	Mine []string `json:"mine"`
	// LostClaims and StrandedHolds come from the sync (see SyncOut).
	LostClaims    []string `json:"lost_claims"`
	StrandedHolds []string `json:"stranded_holds"`
	Stale         int      `json:"stale"`       // idle claims and orphans, all machines
	ReadyCount    int      `json:"ready_count"` // workable candidates for this run
	Synced        bool     `json:"synced"`
	Prime         string   `json:"prime"`
	Warnings      []string `json:"warnings"`
}

// primeMax caps the prime text, which a PRIME.md override can make any size.
const primeMax = 3000

// primeArgs ask bd for its minimal prime: workflow rules and capped memories,
// without the full command reference (`bd prime` prints that on demand).
var primeArgs = []string{"prime", "--mcp", "--max-memory-chars", "600"}

// SessionStart pulls (never pushes), then reports what this run holds, how
// many claims look stale, how much is workable, and bd's prime. A failed
// step is a warning and the rest still runs, so a hook always gets a
// document; a failed sync makes the result ok:false.
func SessionStart(ctx context.Context, env *Env) (*SessionStartOut, error) {
	out := &SessionStartOut{Mine: []string{}, LostClaims: []string{}, StrandedHolds: []string{}, Warnings: []string{}}
	warn := func(what string, err error) {
		out.Warnings = append(out.Warnings, what+": "+firstLine(err.Error()))
	}

	var syncErr error
	if s, err := Sync(ctx, env, true); err != nil {
		syncErr = errors.New("sync failed; the facts below are from this machine's last copy: " + firstLine(err.Error()))
	} else {
		out.Synced = true
		out.LostClaims, out.StrandedHolds = s.LostClaims, s.StrandedHolds
		out.Warnings = append(out.Warnings, s.Warnings...)
	}

	for id := range env.Run.Held {
		out.Mine = append(out.Mine, id)
	}
	sort.Strings(out.Mine)

	if st, err := Stale(ctx, env, StaleOpts{AllMachines: true}); err != nil {
		warn("stale", err)
	} else {
		out.Stale = len(st.Stale) + len(st.Orphans)
	}

	if n, err := Next(ctx, env, NextOpts{N: 1 << 20}); err != nil {
		warn("next", err)
	} else {
		for _, c := range n.Candidates {
			if c.Workable {
				out.ReadyCount++
			}
		}
	}

	if raw, err := env.BD.Run(ctx, primeArgs...); err != nil {
		warn("bd prime", err)
	} else {
		out.Prime = capText(strings.TrimSpace(string(raw)), primeMax)
	}
	return out, syncErr
}

func capText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (cut at " + strconv.Itoa(max) + " bytes; run `bd prime` for the rest)"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
