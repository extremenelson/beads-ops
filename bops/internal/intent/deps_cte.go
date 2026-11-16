package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/extremenelson/beads-ops/bops/internal/depgraph"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
)

// cteID is what engine=cte accepts as a bead id. The id is written into the
// SQL text (bd sql takes no parameters), so anything else is refused before
// bd is asked.
var cteID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// cteRow is one row of the cte query: kind says what a and b hold.
type cteRow struct {
	Kind string          `json:"kind"`
	A    string          `json:"a"`
	B    string          `json:"b"`
	N    json.RawMessage `json:"n"`
}

// cteTarget is the bead a dependencies row points at: an issue, a wisp, or
// an external reference, whichever is set.
const cteTarget = "coalesce(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)"

// cteTargetIs matches a dependencies row whose target is col. It is an OR
// over the three target columns, not cteTarget = col, because each column is
// indexed and the coalesce is not.
func cteTargetIs(col string) string {
	return fmt.Sprintf("(d.depends_on_issue_id = %[1]s or d.depends_on_wisp_id = %[1]s or d.depends_on_external = %[1]s)", col)
}

// cteWaits is the SQL test for a wait edge, and cteNotClosed the test that
// bd does not report the edge's target closed: a target missing from issues
// (a wisp, an external reference) counts as open, as engine=bd counts a
// blocker the list hides.
func cteWaits() string {
	waits := make([]string, 0, len(waitEdges))
	for t := range waitEdges {
		waits = append(waits, "'"+t+"'")
	}
	sort.Strings(waits)
	return "d.type in (" + strings.Join(waits, ", ") + ")"
}

const cteNotClosed = "not exists (select 1 from issues c where c.id = d.depends_on_issue_id and c.status = 'closed')"

// cteUp is the recursive CTE up: the bead and everything it waits on.
func cteUp(lit string) string {
	return "up(id) as (select cast(" + lit + " as char(255)) union select " + cteTarget +
		" from dependencies d join up on d.issue_id = up.id where " + cteWaits() + " and " + cteNotClosed + ")"
}

// cteEdges selects the wait edges among up, as rows of kind edge.
func cteEdges() string {
	return "select distinct 'edge' kind, d.issue_id a, " + cteTarget + " b, 0 n from dependencies d join up on up.id = d.issue_id where " +
		cteWaits() + " and " + cteNotClosed
}

// cteQuery is the one query engine=cte runs. Its recursive CTEs are:
//
//	up    the bead and everything it waits on;
//	down  the bead and the open beads that wait on it, transitively;
//	h     (bead, k) for every chain of k edges inside up that ends at the
//	      bead; k stops at the size of up, which only a cycle reaches.
//
// It returns, per bead in up, the longest chain ending there (dist rows):
// their count is the size of up, their ids the transitive blockers, and a
// chain as long as up is big means a cycle. With path it adds the edges
// among up, and Go walks the longest chain back from the bead with
// engine=bd's tie-break.
//
// Dolt evaluates a CTE again at every reference, and h is the costly one,
// so the query refers to h once. The recursive steps read the dependencies
// table directly rather than a filtered edges CTE, which took over 3s on
// the 1k-node fixture. Walking the path in SQL (a correlated min() per step)
// recomputed h each step and ran past bd's 10s read timeout.
func cteQuery(id string, o DepsOpts) string {
	w, lit := cteWaits(), "'"+id+"'"
	q := "with recursive\n" + cteUp(lit) + ",\n" +
		"down(id) as (select cast(" + lit + " as char(255)) union select d.issue_id from dependencies d join down on " + cteTargetIs("down.id") +
		" join issues i on i.id = d.issue_id where " + w + " and i.status <> 'closed'),\n" +
		"n(c) as (select count(*) from up),\n" +
		"h(id, k) as (select id, 0 from up union select d.issue_id, h.k + 1 from dependencies d join h on " + cteTargetIs("h.id") +
		" join up on up.id = d.issue_id cross join n where " + w + " and h.k < n.c)\n" +
		"select 'status' kind, status a, '' b, 0 n from issues where id = " + lit + "\n" +
		"union all select 'dist', id, '', max(k) from h group by id\n" +
		"union all select distinct 'blocked_by', " + cteTarget + ", '', 0 from dependencies d where d.issue_id = " + lit + " and " + w + " and " + cteNotClosed + "\n" +
		"union all select distinct 'blocks', d.issue_id, '', 0 from dependencies d join issues i on i.id = d.issue_id where " + cteTargetIs(lit) + " and " + w + " and i.status <> 'closed'"
	if o.Transitive {
		q += "\nunion all select 'down', id, '', 0 from down where id <> " + lit
	}
	if o.Path {
		q += "\nunion all " + cteEdges()
	}
	return q
}

// cteCycleQuery returns the wait edges among the bead and what it waits on,
// for naming a cycle when the main query did not fetch them.
func cteCycleQuery(id string) string {
	return "with recursive\n" + cteUp("'"+id+"'") + "\n" + cteEdges()
}

// sqlRows runs one bd sql query. An embedded ledger cannot run bd sql, and
// says so; that becomes an error naming the engine and the way out.
func sqlRows(ctx context.Context, env *Env, q string) ([]cteRow, error) {
	raw, err := env.BD.Run(ctx, "sql", "--json", q)
	if err != nil {
		if strings.Contains(err.Error(), "embedded mode") {
			return nil, fmt.Errorf("deps: engine=cte needs bd in server mode (bd sql does not run on an embedded ledger); use engine=bd")
		}
		if strings.Contains(err.Error(), "invalid connection") {
			return nil, fmt.Errorf("deps: engine=cte: the query outlasted bd's read timeout on the server (%v); use engine=bd", err)
		}
		return nil, err
	}
	var rows []cteRow
	if t := strings.TrimSpace(string(raw)); t != "" && t != "null" {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("deps: engine=cte: unreadable bd sql output: %w", err)
		}
	}
	return rows, nil
}

func rowInt(r cteRow) int {
	var n int
	if json.Unmarshal(r.N, &n) != nil {
		var s string
		if json.Unmarshal(r.N, &s) == nil {
			fmt.Sscan(s, &n)
		}
	}
	return n
}

// depsCTE is engine=cte: the closure and the longest chains from one
// recursive query run by bd sql, so the ledger is not fetched whole. Its
// answer is engine=bd's for the same ledger.
func depsCTE(ctx context.Context, env *Env, id string, o DepsOpts) (*DepsOut, error) {
	if !cteID.MatchString(id) {
		return nil, fmt.Errorf("deps: engine=cte: %q is not a bead id", id)
	}
	rows, err := sqlRows(ctx, env, cteQuery(id, o))
	if err != nil {
		return nil, err
	}
	out := &DepsOut{Blocks: []string{}, BlockedBy: []string{}}
	status := ""
	dist := map[string]int{}
	var down []string
	var edges []cteRow
	for _, r := range rows {
		switch r.Kind {
		case "status":
			status = r.A
		case "dist":
			dist[r.A] = rowInt(r)
		case "blocked_by":
			out.BlockedBy = append(out.BlockedBy, r.A)
		case "blocks":
			out.Blocks = append(out.Blocks, r.A)
		case "down":
			down = append(down, r.A)
		case "edge":
			edges = append(edges, r)
		}
	}
	if status == "" {
		return nil, &ledger.ErrBD{Msg: "no issue " + id}
	}
	if status == "closed" { // engine=bd keeps a closed bead with no edges
		out = &DepsOut{Blocks: []string{}, BlockedBy: []string{}}
		if o.Path {
			out.CriticalPath = []string{id}
		}
		return out, nil
	}
	// A chain inside up as long as up has beads repeats one: a cycle.
	longest := 0
	for _, k := range dist {
		longest = max(longest, k)
	}
	if longest >= len(dist) {
		if !o.Path {
			if edges, err = cteCycleEdges(ctx, env, id); err != nil {
				return nil, err
			}
		}
		return nil, cteCycle(id, edges)
	}
	out.Depth = dist[id]
	if o.Transitive {
		up := make([]string, 0, len(dist))
		for v := range dist {
			if v != id {
				up = append(up, v)
			}
		}
		out.Blocks, out.BlockedBy = sorted(down), sorted(up)
	} else {
		out.Blocks, out.BlockedBy = sorted(out.Blocks), sorted(out.BlockedBy)
	}
	if o.Path {
		// engine=bd's tie-break: from the bead back, the smallest blocker
		// whose own longest chain is one shorter.
		tight := map[string]string{}
		for _, e := range edges {
			if k, ok := dist[e.B]; ok && k == dist[e.A]-1 && (tight[e.A] == "" || e.B < tight[e.A]) {
				tight[e.A] = e.B
			}
		}
		out.CriticalPath = []string{id}
		for v := tight[id]; v != ""; v = tight[v] {
			out.CriticalPath = append(out.CriticalPath, v)
		}
		slices.Reverse(out.CriticalPath)
	}
	return out, nil
}

// cteCycleEdges fetches the wait edges among what id waits on.
func cteCycleEdges(ctx context.Context, env *Env, id string) ([]cteRow, error) {
	return sqlRows(ctx, env, cteCycleQuery(id))
}

// cteCycle names the cycle among edges, the wait edges above id, as
// engine=bd would.
func cteCycle(id string, edges []cteRow) error {
	g := depgraph.New()
	g.Add(id)
	for _, r := range edges {
		if r.Kind == "edge" {
			g.Wait(r.A, r.B)
		}
	}
	if _, err := g.CriticalPath(id); err != nil {
		return err
	}
	return fmt.Errorf("deps: engine=cte found a cycle above %s that its edges do not show", id)
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}
