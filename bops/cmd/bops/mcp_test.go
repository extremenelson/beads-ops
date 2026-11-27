package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

func TestMCPArgv(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	cmd := func(n string) *manifest.Command { c, _ := m.Lookup(n); return c }
	for _, c := range []struct {
		cmd  string
		args map[string]any
		want []string
		in   string
	}{
		{"deps", map[string]any{"id": "x-1", "transitive": true, "engine": "cte"}, []string{"deps", "x-1", "--engine=cte", "--transitive=true", "--json"}, ""},
		{"next", map[string]any{"caps": []any{"go", "sql"}, "n": float64(3)}, []string{"next", "--caps=go,sql", "--n=3", "--json"}, ""},
		{"note", map[string]any{"id": "x-1", "text": "done\nNEXT: ship"}, []string{"note", "x-1", "--json"}, "done\nNEXT: ship"},
		{"release", map[string]any{"id": "x-1", "reason": "done", "text": "why"}, []string{"release", "x-1", "--reason=done", "--file", "-", "--json"}, "why"},
		{"release", map[string]any{"id": "x-1", "reason": "done"}, []string{"release", "x-1", "--reason=done", "--json"}, ""},
		{"version", map[string]any{}, []string{"version", "--json"}, ""},
		// claim's id is optional: claim --next takes none.
		{"claim", map[string]any{"next": true}, []string{"claim", "--next=true", "--json"}, ""},
		{"claim", map[string]any{"id": "x-1"}, []string{"claim", "x-1", "--json"}, ""},
		// An explicit null --only is an empty list (refused), never "no filter".
		{"reap", map[string]any{"dry-run": false, "only": nil}, []string{"reap", "--dry-run=false", "--only=", "--json"}, ""},
		{"reap", map[string]any{"dry-run": false}, []string{"reap", "--dry-run=false", "--json"}, ""},
		{"gc", map[string]any{"dry-run": false, "only": nil}, []string{"gc", "--dry-run=false", "--only=", "--json"}, ""},
	} {
		argv, in, err := mcpArgv(cmd(c.cmd), c.args)
		got := ""
		if in != nil {
			got = *in
		}
		if err != nil || !slices.Equal(argv, c.want) || got != c.in {
			t.Errorf("%s %v: argv %q, stdin %q, err %v; want %q, %q", c.cmd, c.args, argv, got, err, c.want, c.in)
		}
	}
	for _, c := range []struct {
		cmd  string
		args map[string]any
		want string
	}{
		{"deps", map[string]any{}, "needs id"},
		{"note", map[string]any{"id": "x-1"}, "needs text"},
		{"release", map[string]any{"id": "x-1"}, "needs reason"},
		{"deps", map[string]any{"id": "x-1", "verbose": true}, `takes no argument "verbose"`},
		{"deps", map[string]any{"id": "x-1", "path": "yes"}, "must be a boolean"},
		{"next", map[string]any{"n": 2.5}, "must be an integer"},
		{"next", map[string]any{"caps": []any{"a,b"}}, "without commas"},
		{"deps", map[string]any{"id": 7}, "non-empty string"},
		{"claim", map[string]any{"id": "--next"}, "must not start with -"},
	} {
		if _, _, err := mcpArgv(cmd(c.cmd), c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: err %v, want %q", c.cmd, c.args, err, c.want)
		}
	}
}

func TestGenMCPEnableWrites(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, c := range []struct {
		args  []string
		code  int
		tools int
	}{
		{[]string{"gen", "mcp"}, exitVerdict, 8},
		{[]string{"gen", "mcp", "--enable-writes"}, exitVerdict, 16},
		{[]string{"gen", "docs", "--enable-writes"}, exitUsage, -1},
	} {
		stdout, _ := os.CreateTemp(t.TempDir(), "out")
		stderr, _ := os.CreateTemp(t.TempDir(), "err")
		if code := run(c.args, stdout, stderr); code != c.code {
			t.Errorf("bops %v: exit %d, want %d", c.args, code, c.code)
			continue
		}
		if c.tools < 0 {
			continue
		}
		var doc struct{ Tools []json.RawMessage }
		if err := json.Unmarshal(must(os.ReadFile(stdout.Name())), &doc); err != nil || len(doc.Tools) != c.tools {
			t.Errorf("bops %v: %d tools, err %v; want %d", c.args, len(doc.Tools), err, c.tools)
		}
	}
}

// The built binary serves its tools over stdio and runs a call as the
// command; a write tool is unknown without --enable-writes.
func TestServeMCP(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bops")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	t.Chdir(t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, writes := range []bool{false, true} {
		args := []string{"serve-mcp"}
		if writes {
			args = append(args, "--enable-writes")
		}
		cmd := exec.Command(bin, args...)
		cmd.Stdin = strings.NewReader(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"deps","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"claim","arguments":{"id":"x-1","force":true}}}`,
		}, "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("serve-mcp %v: %v", args, err)
		}
		var resps []map[string]any
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		for sc.Scan() {
			var r map[string]any
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("stdout line %q: %v", sc.Text(), err)
			}
			resps = append(resps, r)
		}
		if len(resps) != 4 {
			t.Fatalf("writes=%v: %d responses: %s", writes, len(resps), out)
		}
		tools := resps[1]["result"].(map[string]any)["tools"].([]any)
		if want := map[bool]int{false: 8, true: 16}[writes]; len(tools) != want {
			t.Errorf("writes=%v: %d tools, want %d", writes, len(tools), want)
		}
		res := resps[2]["result"].(map[string]any)
		if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "needs id") {
			t.Errorf("deps without id = %v", res)
		}
		if writes {
			// The call reached bops: with no git repo it cannot run, and says so.
			if r := resps[3]["result"].(map[string]any); r["isError"] != true {
				t.Errorf("claim = %v", r)
			}
		} else if e, _ := resps[3]["error"].(map[string]any); e == nil || !strings.Contains(e["message"].(string), "unknown tool claim") {
			t.Errorf("claim without --enable-writes = %v", resps[3])
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
