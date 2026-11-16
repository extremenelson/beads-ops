package intent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/depgraph"
	"github.com/extremenelson/beads-ops/bops/internal/depgraph/depgraphtest"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// On an embedded ledger, which is what liveLedger makes, engine=cte refuses
// and says why.
func TestLiveDepsCTEOnEmbeddedLedger(t *testing.T) {
	env, create := liveLedger(t)
	id := create("one")
	_, err := intent.Deps(context.Background(), env, id, intent.DepsOpts{Engine: "cte"})
	if err == nil || !strings.Contains(err.Error(), "needs bd in server mode") {
		t.Fatalf("err = %v", err)
	}
}

// serverLedger makes a throwaway ledger on the dolt sql-server at
// BOPS_DOLT_SERVER (host:port, user root, no password), in a database of its
// own, holding the beads ids, each waiting on others along edges. It skips
// without BOPS_INTEGRATION=1 and BOPS_DOLT_SERVER.
//
// bd import creates the beads without dependencies, and the edges go in by
// bd sql, in batches. Importing the edges makes bd recompute is_blocked per
// chunk, which outlasted bd's 10s read timeout ("invalid connection") on
// every CI runner; and bd refuses an edge that closes a cycle, which a
// ledger only gets when replicas that each added an edge merge. Both
// engines read the same dependencies rows.
func serverLedger(t *testing.T, ids []string, edges []depgraphtest.Edge) *intent.Env {
	t.Helper()
	if os.Getenv("BOPS_INTEGRATION") != "1" {
		t.Skip("set BOPS_INTEGRATION=1 to run against a real bd")
	}
	addr := os.Getenv("BOPS_DOLT_SERVER")
	if addr == "" {
		t.Skip("set BOPS_DOLT_SERVER=host:port to run against a dolt sql-server")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("BOPS_DOLT_SERVER=%q: %v", addr, err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("BEADS_DIR", "")
	t.Setenv("BEADS_DB", "")
	t.Setenv("BD_NON_INTERACTIVE", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sh := func(stdin []byte, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Stdin = bytes.NewReader(stdin)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	sh(nil, "git", "init", "-q")
	// The prefix names the server database, so each run gets its own.
	letters := []byte("abcdefghijklmnopqrstuvwxyz")
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	prefix := "cte"
	for range 6 {
		prefix += string(letters[r.Intn(len(letters))])
	}
	sh(nil, "bd", "init", "--quiet", "--prefix", prefix, "--server", "--external",
		"--server-host", host, "--server-port", port, "--server-user", "root", "--sandbox")

	var jsonl bytes.Buffer
	enc := json.NewEncoder(&jsonl)
	for _, id := range ids {
		if err := enc.Encode(map[string]any{"id": id, "title": id, "status": "open", "priority": 2, "issue_type": "task"}); err != nil {
			t.Fatal(err)
		}
	}
	sh(jsonl.Bytes(), "bd", "import", "-")

	seen := map[depgraphtest.Edge]bool{}
	var rows []string
	flush := func() {
		if len(rows) == 0 {
			return
		}
		q := "insert into dependencies (id, issue_id, type, created_by, depends_on_issue_id) values " + strings.Join(rows, ", ")
		if out, err := (bdx.Exec{}).Run(context.Background(), "sql", q); err != nil {
			t.Fatalf("writing edges: %v\n%s", err, out)
		}
		rows = rows[:0]
	}
	for _, e := range edges {
		if seen[e] {
			continue
		}
		seen[e] = true
		rows = append(rows, fmt.Sprintf("(uuid(), '%s', 'blocks', 'it', '%s')", e.Blocked, e.Blocker))
		if len(rows) == 500 { // well under Linux's 128 KiB limit on one argument
			flush()
		}
	}
	flush()
	return newEnv(bdx.Exec{})
}

// engine=cte answers as engine=bd does on the 1k-node fixture, read straight
// from the ledger, and names the same cycle.
func TestLiveDepsCTEMatchesBD(t *testing.T) {
	ids, edges, chain := depgraphtest.Thousand("ct-b")
	// A cycle beside the DAG, and a bead that waits on it.
	ids = append(ids, "ct-c1", "ct-c2", "ct-c3", "ct-c4")
	edges = append(edges,
		depgraphtest.Edge{Blocked: "ct-c1", Blocker: "ct-c2"},
		depgraphtest.Edge{Blocked: "ct-c2", Blocker: "ct-c3"},
		depgraphtest.Edge{Blocked: "ct-c3", Blocker: "ct-c1"},
		depgraphtest.Edge{Blocked: "ct-c4", Blocker: "ct-c2"})
	env := serverLedger(t, ids, edges)
	ctx := context.Background()

	// The planted chain's end, its start, a middle bead, and a spread of
	// others: every combination of --transitive and --path.
	sample := []string{chain[len(chain)-1], chain[0], chain[25]}
	for i := 7; i < len(ids); i += 97 {
		sample = append(sample, ids[i])
	}
	var slowest time.Duration
	for _, id := range sample {
		for _, o := range []intent.DepsOpts{{}, {Transitive: true, Path: true}} {
			o.Engine = "bd"
			want, err := intent.Deps(ctx, env, id, o)
			if err != nil {
				t.Fatalf("%s engine=bd: %v", id, err)
			}
			o.Engine = "cte"
			start := time.Now()
			got, err := intent.Deps(ctx, env, id, o)
			slowest = max(slowest, time.Since(start))
			if err != nil {
				t.Fatalf("%s engine=cte: %v", id, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %+v:\ncte %+v\nbd  %+v", id, o, got, want)
			}
		}
	}
	end, err := intent.Deps(ctx, env, chain[len(chain)-1], intent.DepsOpts{Engine: "cte", Path: true})
	if err != nil || end.Depth != len(chain)-1 || !reflect.DeepEqual(end.CriticalPath, chain) {
		t.Errorf("planted chain: depth %d, path %v, err %v", end.Depth, end.CriticalPath, err)
	}

	for _, id := range []string{"ct-c2", "ct-c4"} {
		var bdErr, cteErr *depgraph.CycleError
		_, e1 := intent.Deps(ctx, env, id, intent.DepsOpts{Engine: "bd"})
		_, e2 := intent.Deps(ctx, env, id, intent.DepsOpts{Engine: "cte"})
		if !errors.As(e1, &bdErr) || !errors.As(e2, &cteErr) || e1.Error() != e2.Error() {
			t.Errorf("%s: engine=bd %v, engine=cte %v", id, e1, e2)
		}
	}
	t.Logf("engine=cte matched engine=bd on %d beads; slowest cte call %s (bd sql's read timeout is 10s)", len(sample), slowest.Round(time.Millisecond))
}
