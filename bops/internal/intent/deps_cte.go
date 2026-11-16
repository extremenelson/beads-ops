package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// cteQuery is the one query engine=cte runs. Its recursive CTEs are:
//
//	up    the bead and everything it waits on, following wait edges whose
//	      target bd does not report closed (a target missing from issues,
//	      a wisp or an external reference, counts as open, as engine=bd
//	      counts a blocker the list hides);
//	down  the bead and the open beads that wait on it, transitively;
//	h     (bead, k) for every chain of k edges inside up that ends at the
//	      bead; k stops at the size of up, which only a cycle reaches;
//	dist  the longest such chain per bead.
//
// Dolt re-evaluates a CTE wherever it is referenced, so the recursive steps
// read the dependencies table directly rather than a filtered edges CTE, and
// the path is not walked in SQL: a correlated min() over dist per step
// recomputes h each time and runs past bd's 10s read timeout on the 1k-node
// fixture. Instead the query returns the tight edges (blocker one shorter
// than the bead waiting on it), and Go walks them with engine=bd's
// tie-break.
func cteQuery(id string, o DepsOpts) string {
	waits := make([]string, 0, len(waitEdges))
	for t := range waitEdges {
		waits = append(waits, "'"+t+"'")
	}
	sort.Strings(waits)
	w := "d.type in (" + strings.Join(waits, ", ") + ")"
	lit := "'" + id + "'"
	notClosed := "not exists (select 1 from issues c where c.id = d.depends_on_issue_id and c.status = 'closed')"

	q := "with recursive\n" +
		"up(id) as (select cast(" + lit + " as char(255)) union select " + cteTarget +
		" from dependencies d join up on d.issue_id = up.id where " + w + " and " + notClosed + "),\n" +
		"down(id) as (select cast(" + lit + " as char(255)) union select d.issue_id from dependencies d join down on " + cteTargetIs("down.id") +
		" join issues i on i.id = d.issue_id where " + w + " and i.status <> 'closed'),\n" +
		"n(c) as (select count(*) from up),\n" +
		"h(id, k) as (select id, 0 from up union select d.issue_id, h.k + 1 from dependencies d join h on " + cteTargetIs("h.id") +
		" join up on up.id = d.issue_id cross join n where " + w + " and h.k < n.c),\n" +
		"dist(id, k) as (select id, max(k) from h group by id)\n" +
		"select 'status' kind, status a, '' b, 0 n from issues where id = " + lit + "\n" +
		"union all select 'size', '', '', c from n\n" +
		"union all select 'longest', '', '', max(k) from h\n" +
		"union all select 'depth', '', '', k from dist where id = " + lit + "\n" +
		"union all select distinct 'blocked_by', " + cteTarget + ", '', 0 from dependencies d where d.issue_id = " + lit + " and " + w + " and " + notClosed + "\n" +
		"union all select distinct 'blocks', d.issue_id, '', 0 from dependencies d join issues i on i.id = d.issue_id where " + cteTargetIs(lit) + " and " + w + " and i.status <> 'closed'"
	if o.Transitive {
		q += "\nunion all select 'up', id, '', 0 from up where id <> " + lit +
			"\nunion all select 'down', id, '', 0 from down where id <> " + lit
	}
	if o.Path {
		q += "\nunion all select distinct 'tight', d.issue_id, " + cteTarget + ", 0 from dependencies d join dist dv on dv.id = d.issue_id join dist du on du.id = " + cteTarget +
			" where " + w + " and du.k = dv.k - 1"
	}
	return q
}

// cteCycleQuery returns the wait edges among the bead and what it waits on,
// for naming a cycle the main query found.
func cteCycleQuery(id string) string {
	waits := make([]string, 0, len(waitEdges))
	for t := range waitEdges {
		waits = append(waits, "'"+t+"'")
	}
	sort.Strings(waits)
	w := "d.type in (" + strings.Join(waits, ", ") + ")"
	lit := "'" + id + "'"
	notClosed := "not exists (select 1 from issues c where c.id = d.depends_on_issue_id and c.status = 'closed')"
	return "with recursive\n" +
		"up(id) as (select cast(" + lit + " as char(255)) union select " + cteTarget +
		" from dependencies d join up on d.issue_id = up.id where " + w + " and " + notClosed + ")\n" +
		"select distinct 'edge' kind, d.issue_id a, " + cteTarget + " b, 0 n from dependencies d join up on up.id = d.issue_id where " + w + " and " + notClosed
}

// sqlRows runs one bd sql query. An embedded ledger cannot run bd sql, and
// says so; that becomes an error naming the engine and the way out.
func sqlRows(ctx context.Context, env *Env, q string) ([]cteRow, error) {
	raw, err := env.BD.Run(ctx, "sql", "--json", q)
	if err != nil {
		if strings.Contains(err.Error(), "embedded mode") {
			return nil, fmt.Errorf("deps: engine=cte needs bd in server mode (bd sql does not run on an embedded ledger); use engine=bd")
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

// depsCTE is engine=cte: the closure and the longest chain from one
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
	status, size, longest := "", 0, 0
	var up, down []string
	tight := map[string][]string{}
	for _, r := range rows {
		switch r.Kind {
		case "status":
			status = r.A
		case "size":
			size = rowInt(r)
		case "longest":
			longest = rowInt(r)
		case "depth":
			out.Depth = rowInt(r)
		case "blocked_by":
			out.BlockedBy = append(out.BlockedBy, r.A)
		case "blocks":
			out.Blocks = append(out.Blocks, r.A)
		case "up":
			up = append(up, r.A)
		case "down":
			down = append(down, r.A)
		case "tight":
			tight[r.A] = append(tight[r.A], r.B)
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
	if size > 0 && longest >= size {
		return nil, cteCycle(ctx, env, id)
	}
	if o.Transitive {
		out.Blocks, out.BlockedBy = sorted(down), sorted(up)
	} else {
		out.Blocks, out.BlockedBy = sorted(out.Blocks), sorted(out.BlockedBy)
	}
	if o.Path {
		// engine=bd's tie-break: from the bead back, the smallest blocker
		// whose own longest chain is one shorter.
		out.CriticalPath = []string{id}
		for v := id; len(tight[v]) > 0; {
			v = slicesMin(tight[v])
			out.CriticalPath = append(out.CriticalPath, v)
		}
		for i, j := 0, len(out.CriticalPath)-1; i < j; i, j = i+1, j-1 {
			out.CriticalPath[i], out.CriticalPath[j] = out.CriticalPath[j], out.CriticalPath[i]
		}
	}
	return out, nil
}

// cteCycle fetches the wait edges among what id waits on and names the
// cycle there as engine=bd would.
func cteCycle(ctx context.Context, env *Env, id string) error {
	rows, err := sqlRows(ctx, env, cteCycleQuery(id))
	if err != nil {
		return err
	}
	g := depgraph.New()
	g.Add(id)
	for _, r := range rows {
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

func slicesMin(s []string) string {
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
