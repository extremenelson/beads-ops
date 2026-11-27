package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// ledgerDir is configDir with a .beads directory, so guard treats the
// directory as a beads ledger.
func ledgerDir(t *testing.T, repoJSON string) string {
	t.Helper()
	configDir(t, repoJSON)
	dir, _ := os.Getwd()
	if err := os.Mkdir(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// otherLedger makes a second ledger directory, with .bops.json set to
// repoJSON.
func otherLedger(t *testing.T, repoJSON string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bops.json"), []byte(repoJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func payload(t *testing.T, tool, command, cwd string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": map[string]string{"command": command}, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runGuard runs `bops guard args...` with stdin and returns its exit code,
// stdout and stderr.
func runGuard(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _ := os.CreateTemp(t.TempDir(), "out")
	stderr, _ := os.CreateTemp(t.TempDir(), "err")
	code := cmdGuard(m, args, true, strings.NewReader(stdin), false, stdout, stderr)
	return code, string(must(os.ReadFile(stdout.Name()))), string(must(os.ReadFile(stderr.Name())))
}

type denial struct {
	H struct {
		Event    string `json:"hookEventName"`
		Decision string `json:"permissionDecision"`
		Reason   string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// With the lock on, a raw claim gets a deny decision naming the bops command
// to use, and exit 0 so Claude Code reads it.
func TestGuardHookDeniesRawClaims(t *testing.T) {
	dir := ledgerDir(t, "")
	for cmd, use := range map[string]string{
		"bd update bops-1 --claim": "bops claim <id>, or bops claim --force <id>",
		"bd reclaim":               "bops claim --force <id>",
		"bd ready --claim --json":  "bops next, then bops claim <id>",
	} {
		code, out, _ := runGuard(t, payload(t, "Bash", cmd, dir), "--hook-json")
		var d denial
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatalf("%q: %v: %q", cmd, err, out)
		}
		if code != exitVerdict || d.H.Event != "PreToolUse" || d.H.Decision != "deny" || !strings.Contains(d.H.Reason, use) {
			t.Errorf("%q: exit %d, %+v; want deny naming %q", cmd, code, d.H, use)
		}
	}
}

// Anything else prints nothing (never an allow decision, which would skip
// the permission prompt) and exits 0: other commands, other tools, and raw
// claims while lock.mode is none.
func TestGuardHookAllows(t *testing.T) {
	on := ledgerDir(t, "")
	off := otherLedger(t, `{"lock":{"mode":"none"}}`)
	notLedger := t.TempDir()
	for name, in := range map[string]string{
		"bops claim":        payload(t, "Bash", "bops claim bops-1", on),
		"other bd update":   payload(t, "Bash", "bd update bops-1 --status open", on),
		"quoted":            payload(t, "Bash", "git commit -m 'never bd reclaim'", on),
		"another tool":      payload(t, "Edit", "bd reclaim", on),
		"lock off: update":  payload(t, "Bash", "bd update bops-1 --claim", off),
		"lock off: reclaim": payload(t, "Bash", "bd reclaim", off),
		"no .beads":         payload(t, "Bash", "bd reclaim", notLedger),
		"help":              payload(t, "Bash", "bd reclaim --help", on),
	} {
		if code, out, _ := runGuard(t, in, "--hook-json"); code != exitVerdict || out != "" {
			t.Errorf("%s: exit %d, stdout %q; want 0 and nothing", name, code, out)
		}
	}
}

// Whatever stops the guard deciding allows the command: a payload that is
// not JSON, an empty one, a config bops refuses, and bad flags.
func TestGuardHookFailsOpen(t *testing.T) {
	on := ledgerDir(t, "")
	bad := otherLedger(t, `{"lock":{"mode":"bogus"}}`)
	for name, tc := range map[string]struct {
		stdin string
		args  []string
	}{
		"not json":   {"bd reclaim", []string{"--hook-json"}},
		"empty":      {"", []string{"--hook-json"}},
		"bad config": {payload(t, "Bash", "bd reclaim", bad), []string{"--hook-json"}},
		"bad flag":   {payload(t, "Bash", "bd reclaim", on), []string{"--hook-json", "--nope"}},
		"oversized":  {`{"tool_name":"Bash","tool_input":{"command":"bd reclaim` + strings.Repeat(" ", guardMaxInput) + `"}}`, []string{"--hook-json"}},
	} {
		if code, out, _ := runGuard(t, tc.stdin, tc.args...); code != exitVerdict || out != "" {
			t.Errorf("%s: exit %d, stdout %q; want 0 and nothing", name, code, out)
		}
	}
}

// Without --hook-json, guard reads the command as text and answers as a
// read: exit 0 with refused, matched and use; a config it cannot load allows
// with a warning.
func TestGuardPlain(t *testing.T) {
	ledgerDir(t, "")
	code, out, _ := runGuard(t, "bd update x --claim\n")
	var got GuardOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if code != exitVerdict || !got.Refused || got.Matched != "bd update --claim" || !strings.HasPrefix(got.Use, "bops claim") {
		t.Fatalf("exit %d: %+v", code, got)
	}

	code, out, _ = runGuard(t, "bd show x")
	got = GuardOut{}
	_ = json.Unmarshal([]byte(out), &got)
	if code != exitVerdict || got.Refused || got.Matched != "" {
		t.Fatalf("exit %d: %+v", code, got)
	}

	if err := os.WriteFile(".bops.json", []byte(`{"lock":{"mode":"bogus"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runGuard(t, "bd reclaim")
	got = GuardOut{}
	_ = json.Unmarshal([]byte(out), &got)
	if code != exitVerdict || got.Refused || got.Matched != "bd reclaim" || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "allowed") {
		t.Fatalf("bad config: exit %d: %+v", code, got)
	}

	if code, _, _ := runGuard(t, "", "--hook-json", "--file", "x"); code != exitVerdict {
		t.Errorf("--hook-json with --file: exit %d, want 0 (a hook that cannot run allows)", code)
	}
	if code, _, _ := runGuard(t, "", "--file", filepath.Join(t.TempDir(), "missing")); code != exitUsage {
		t.Errorf("missing --file: exit %d, want usage", code)
	}
}

// The generated hook command, run by a shell as Claude Code runs it: with
// bops on PATH it denies a raw claim; with no bops it exits 0 and prints
// nothing, so the call goes ahead.
func TestGeneratedGuardHook(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	files, err := gen.CC(m)
	if err != nil {
		t.Fatal(err)
	}
	var hookCmd string
	for _, f := range files {
		if f.Path == "guard-hooks.json" {
			var h struct {
				Hooks map[string][]struct {
					Matcher string
					Hooks   []struct{ Command string }
				}
			}
			if err := json.Unmarshal(f.Data, &h); err != nil {
				t.Fatal(err)
			}
			g := h.Hooks["PreToolUse"]
			if len(h.Hooks) != 1 || len(g) != 1 || g[0].Matcher != "Bash" || len(g[0].Hooks) != 1 {
				t.Fatalf("guard-hooks.json = %s", f.Data)
			}
			hookCmd = g[0].Hooks[0].Command
		}
	}
	if hookCmd == "" {
		t.Fatal("gen cc wrote no guard-hooks.json")
	}

	binDir := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(binDir, "bops"), ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dir := ledgerDir(t, "")
	shell := func(path string) (int, string) {
		c := exec.Command("/bin/sh", "-c", hookCmd)
		c.Env = append(os.Environ(), "PATH="+path)
		c.Stdin = strings.NewReader(payload(t, "Bash", "bd reclaim", dir))
		out, err := c.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, string(out)
	}
	if code, out := shell(binDir); code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("with bops: exit %d, stdout %q; want a deny", code, out)
	}
	if code, out := shell(t.TempDir()); code != 0 || out != "" {
		t.Errorf("without bops: exit %d, stdout %q; want 0 and nothing (fail open)", code, out)
	}
}

// With lock.mode none, a raw claim is matched but allowed, and the result
// says why; without .beads it is allowed as not a ledger.
func TestGuardPlainAllowsWithAReason(t *testing.T) {
	ledgerDir(t, `{"lock":{"mode":"none"}}`)
	_, out, _ := runGuard(t, "bd reclaim")
	var got GuardOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if got.Refused || got.Matched != "bd reclaim" || len(got.Warnings) != 1 || got.Warnings[0] != lock.OffWarning {
		t.Fatalf("lock off: %+v", got)
	}

	configDir(t, "")
	_, out, _ = runGuard(t, "bd reclaim")
	got = GuardOut{}
	_ = json.Unmarshal([]byte(out), &got)
	if got.Refused || got.Matched != "bd reclaim" || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "no .beads") {
		t.Fatalf("no .beads: %+v", got)
	}
}

// A linked worktree with no .beads of its own (a gitignored ledger) is still
// guarded when its main checkout has one.
func TestGuardFindsTheLedgerOfALinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	configDir(t, "")
	mainDir, _ := os.Getwd()
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(mainDir, "init", "-q")
	git(mainDir, "commit", "-q", "--allow-empty", "-m", "x")
	if err := os.Mkdir(filepath.Join(mainDir, ".beads"), 0o755); err != nil { // untracked, as when gitignored
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(mainDir, "worktree", "add", "-q", wt)
	code, out, _ := runGuard(t, payload(t, "Bash", "bd reclaim", wt), "--hook-json")
	if code != exitVerdict || !strings.Contains(out, `"deny"`) {
		t.Fatalf("exit %d, stdout %q; want a deny", code, out)
	}
}
