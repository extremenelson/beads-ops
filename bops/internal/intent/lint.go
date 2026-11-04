package intent

import (
	"fmt"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
)

// Rule is one workability rule result (SPEC.md §6).
type Rule struct {
	ID   string `json:"id"`
	Rule string `json:"rule"`
}

// LintResult is the verdict for one bead.
type LintResult struct {
	ID       string   `json:"id"`
	Workable bool     `json:"workable"`
	Fails    []Rule   `json:"fails"`
	Warns    []Rule   `json:"warns"`
	Passes   []string `json:"passes"`
}

// LintEnv is what the rules need beyond the bead itself.
type LintEnv struct {
	Pool        string
	Caps        map[string]bool
	Now         time.Time
	StaleAfter  time.Duration // a run: label with no heartbeat for this long is stale
	MaxBodySize int
	Strict      bool
	// OpenGates maps bead id -> ids of unsatisfied gates (nil when unknown).
	OpenGates map[string][]string
}

const defaultMaxBody = 8 * 1024

// Lint evaluates rules W01–W11 against one bead. W11 (cycles) is delegated
// to bd at claim time and reported here only when OpenGates reports it.
func Lint(i *ledger.Issue, env LintEnv) LintResult {
	r := LintResult{ID: i.ID, Fails: []Rule{}, Warns: []Rule{}, Passes: []string{}}
	fail := func(id, msg string) { r.Fails = append(r.Fails, Rule{id, msg}) }
	warn := func(id, msg string) {
		if env.Strict {
			r.Fails = append(r.Fails, Rule{id, msg})
		} else {
			r.Warns = append(r.Warns, Rule{id, msg})
		}
	}
	pass := func(id string) { r.Passes = append(r.Passes, id) }

	if strings.TrimSpace(i.AcceptanceCriteria) == "" {
		fail("W01", "no acceptance criteria")
	} else {
		pass("W01")
	}
	switch i.Status {
	case "open", "ready":
		pass("W02")
	default:
		fail("W02", "status "+i.Status+" does not permit work")
	}
	if i.HasLabel("needs:human") {
		fail("W03", "labelled needs:human")
	} else {
		pass("W03")
	}
	if unmet := unmetCaps(i, env.Caps); len(unmet) > 0 {
		fail("W04", "needs capabilities this run lacks: "+strings.Join(unmet, ","))
	} else {
		pass("W04")
	}
	if g := env.OpenGates[i.ID]; len(g) > 0 {
		fail("W05", "open gates: "+strings.Join(g, ","))
	} else {
		pass("W05")
	}
	if runs := i.LabelsWithPrefix("run:"); len(runs) > 0 || i.Assignee != "" {
		if isStale(i, env) {
			warn("W06s", "claimed by a stale run ("+describeClaim(i)+")")
		} else {
			fail("W06", "claimed by a live run ("+describeClaim(i)+")")
		}
	} else {
		pass("W06")
	}
	if i.Parent == "" && i.IssueType != "epic" {
		warn("W07", "no parent epic")
	} else {
		pass("W07")
	}
	max := env.MaxBodySize
	if max == 0 {
		max = defaultMaxBody
	}
	if n := len(i.Description); n > max {
		warn("W08", fmt.Sprintf("body is %d bytes (> %d)", n, max))
	} else {
		pass("W08")
	}
	// W09 (NEXT: paragraph) and W10 (duplicate title) need comments / the
	// full list and are evaluated by callers that have them; W11 by bd.
	if i.HasLabel("bops:hold") {
		fail("W12", "labelled bops:hold (cooling off)")
	} else {
		pass("W12")
	}
	// A run serves exactly one pool; unlabelled beads are the default pool.
	if env.Pool != "" {
		pools := i.LabelsWithPrefix("pool:")
		if !contains(pools, env.Pool) {
			fail("W13", "not in pool "+env.Pool+" (bead pools: "+strings.Join(pools, ",")+")")
		} else {
			pass("W13")
		}
	} else if pools := i.LabelsWithPrefix("pool:"); len(pools) > 0 {
		fail("W13", "bead is in pool "+strings.Join(pools, ",")+"; this run serves the default pool")
	} else {
		pass("W13")
	}
	r.Workable = len(r.Fails) == 0
	return r
}

func unmetCaps(i *ledger.Issue, have map[string]bool) []string {
	var out []string
	for _, c := range i.LabelsWithPrefix("needs:") {
		if c == "human" {
			continue
		}
		if !have[c] {
			out = append(out, c)
		}
	}
	return out
}

func isStale(i *ledger.Issue, env LintEnv) bool {
	if env.StaleAfter == 0 {
		return false
	}
	last := i.UpdatedAt
	if i.HeartbeatAt != nil && i.HeartbeatAt.After(last) {
		last = *i.HeartbeatAt
	}
	if i.LeaseExpiresAt != nil && i.LeaseExpiresAt.Before(env.Now) && env.Now.Sub(last) > env.StaleAfter {
		return true
	}
	return env.Now.Sub(last) > env.StaleAfter
}

func describeClaim(i *ledger.Issue) string {
	parts := []string{}
	if i.Assignee != "" {
		parts = append(parts, "assignee="+i.Assignee)
	}
	for _, r := range i.LabelsWithPrefix("run:") {
		parts = append(parts, "run="+r)
	}
	return strings.Join(parts, " ")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
