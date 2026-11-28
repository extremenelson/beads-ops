package main

import (
	"bufio"
	"encoding/json"
	"errors"
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
		{"note", map[string]any{"id": "x-1", "text": "t", "op-id": "op-1"}, []string{"note", "x-1", "--op-id=op-1", "--json"}, "t"},
		{"release", map[string]any{"id": "x-1", "reason": "scope", "op-id": "op-2"}, []string{"release", "x-1", "--op-id=op-2", "--reason=scope", "--json"}, ""},
		{"version", map[string]any{}, []string{"version", "--json"}, ""},
		// claim's id is optional: claim --next takes none.
		{"claim", map[string]any{"next": true}, []string{"claim", "--next=true", "--json"}, ""},
		{"claim", map[string]any{"id": "x-1"}, []string{"claim", "x-1", "--json"}, ""},
		// An explicit null --only is an empty list (refused), never "no filter".
		{"reap", map[string]any{"dry-run": true, "only": nil}, []string{"reap", "--dry-run=true", "--only=", "--json"}, ""},
		{"gc", map[string]any{"only": nil}, []string{"gc", "--dry-run=true", "--only=", "--json"}, ""},
		// reap and gc preview only over MCP: the dry run is always passed.
		{"reap", map[string]any{"only": "x-1@01ABC"}, []string{"reap", "--dry-run=true", "--only=x-1@01ABC", "--json"}, ""},
		{"reap", nil, []string{"reap", "--dry-run=true", "--json"}, ""},
		{"gc", map[string]any{"dry-run": true}, []string{"gc", "--dry-run=true", "--json"}, ""},
		{"reap", map[string]any{"dry-run": nil}, []string{"reap", "--dry-run=true", "--json"}, ""},
		// sweep previews only over MCP too.
		{"sweep", map[string]any{"forge": "none"}, []string{"sweep", "--dry-run=true", "--forge=none", "--json"}, ""},
		{"sweep", map[string]any{"dry-run": true}, []string{"sweep", "--dry-run=true", "--json"}, ""},
		// claim's force is not on the tool: its default or null is dropped.
		{"claim", map[string]any{"id": "x-1", "force": false}, []string{"claim", "x-1", "--json"}, ""},
		{"claim", map[string]any{"id": "x-1", "force": nil}, []string{"claim", "x-1", "--json"}, ""},
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
		// A real reap or gc is a person's, at the CLI (PROCESS §12).
		{"reap", map[string]any{"dry-run": false, "only": "x-1@01ABC"}, "bops reap --dry-run=false --only <only>,... (each would_release entry's only: <id>@<run>, or the bare id when its run cannot be pinned; skip an entry without one)"},
		{"reap", map[string]any{"dry-run": false}, "a real run is a person's decision"},
		{"reap", map[string]any{"dry-run": "false"}, "dry-run must be true"},
		{"gc", map[string]any{"dry-run": false, "only": "x-1"}, "bops gc --dry-run=false --only"},
		// A real sweep closes beads: a person's or a scheduled job's, at the CLI.
		{"sweep", map[string]any{"dry-run": false}, "Hand them the command: bops sweep --dry-run=false"},
		// A takeover is a person's, at the CLI (PROCESS §12).
		{"claim", map[string]any{"id": "x-1", "force": true}, "force is a person's decision"},
		{"claim", map[string]any{"id": "x-1", "force": "true"}, "force is a person's decision"},
	} {
		if _, _, err := mcpArgv(cmd(c.cmd), c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: err %v, want %q", c.cmd, c.args, err, c.want)
		}
	}
	// sweep takes no --only, so the command handed to a person has none.
	if _, _, err := mcpArgv(cmd("sweep"), map[string]any{"dry-run": false}); err == nil || strings.Contains(err.Error(), "--only") {
		t.Errorf("sweep dry-run false: err %v, want one naming no --only", err)
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
		{[]string{"gen", "mcp"}, exitVerdict, 11},
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
			`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"reap","arguments":{"dry-run":false,"only":"x-1"}}}`,
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
		if len(resps) != 5 {
			t.Fatalf("writes=%v: %d responses: %s", writes, len(resps), out)
		}
		tools := resps[1]["result"].(map[string]any)["tools"].([]any)
		if want := map[bool]int{false: 11, true: 16}[writes]; len(tools) != want {
			t.Errorf("writes=%v: %d tools, want %d", writes, len(tools), want)
		}
		res := resps[2]["result"].(map[string]any)
		if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "needs id") {
			t.Errorf("deps without id = %v", res)
		}
		// Over MCP a takeover is a person's: with the write tools claim
		// refuses force before bops runs; without them claim is no tool.
		if writes {
			r := resps[3]["result"].(map[string]any)
			if text := r["content"].([]any)[0].(map[string]any)["text"].(string); r["isError"] != true || !strings.Contains(text, "force is a person's decision") {
				t.Errorf("claim force = %v", r)
			}
		} else if e, _ := resps[3]["error"].(map[string]any); e == nil || !strings.Contains(e["message"].(string), "unknown tool claim") {
			t.Errorf("claim without --enable-writes = %v", resps[3])
		}
		// With or without the write tools, reap only previews: a real run is
		// a person's, at the CLI, and is refused before bops runs.
		r := resps[4]["result"].(map[string]any)
		if text := r["content"].([]any)[0].(map[string]any)["text"].(string); r["isError"] != true || !strings.Contains(text, "a real run is a person's decision") {
			t.Errorf("writes=%v: reap dry-run false = %v", writes, r)
		}
		previews := 0
		for _, tl := range tools {
			tool := tl.(map[string]any)
			props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if tool["name"] == "claim" && props["force"] != nil {
				t.Errorf("claim tool takes force: %v", tool)
			}
			if tool["name"] != "reap" && tool["name"] != "gc" && tool["name"] != "sweep" {
				continue
			}
			previews++
			dry, _ := props["dry-run"].(map[string]any)
			if dry == nil || !strings.Contains(dry["description"].(string), "Only true") || !strings.Contains(tool["description"].(string), "Preview only here") || strings.Contains(tool["description"].(string), "Writes:") {
				t.Errorf("%s tool does not say it previews only: %v", tool["name"], tool)
			}
			if tool["annotations"].(map[string]any)["readOnlyHint"] != true {
				t.Errorf("writes=%v: %s tool is not readOnlyHint: %v", writes, tool["name"], tool)
			}
		}
		if previews != 3 {
			t.Errorf("writes=%v: %d of reap, gc and sweep served, want 3", writes, previews)
		}
	}
	// The CLI still takes a real run: it gets past its flags to the repo
	// check (exit 1 here, with no git repo), not a refusal or a usage error.
	for _, c := range [][]string{{"reap", "--only", "x-1"}, {"gc", "--only", "x-1"}, {"sweep"}} {
		cmd := exec.Command(bin, append(append([]string{c[0], "--dry-run=false"}, c[1:]...), "--json")...)
		out, err := cmd.Output()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != exitCantRun || strings.Contains(string(out), "person's decision") {
			t.Errorf("bops %s --dry-run=false: err %v, out %s; want exit %d past the flags", c[0], err, out, exitCantRun)
		}
	}
	// The CLI still takes claim --force: past its flags to the repo check.
	out, err := exec.Command(bin, "claim", "x-1", "--force", "--json").Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != exitCantRun || strings.Contains(string(out), "person's decision") {
		t.Errorf("bops claim --force: err %v, out %s; want exit %d past the flags", err, out, exitCantRun)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
