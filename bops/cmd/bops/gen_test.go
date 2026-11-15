package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runGen(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	stdout, _ := os.CreateTemp(t.TempDir(), "out")
	stderr, _ := os.CreateTemp(t.TempDir(), "err")
	code := run(append([]string{"gen"}, args...), stdout, stderr)
	o, _ := os.ReadFile(stdout.Name())
	e, _ := os.ReadFile(stderr.Name())
	return code, string(o), string(e)
}

// A target that writes several files needs a directory to write them to.
func TestGenMultiFileTargetNeedsOut(t *testing.T) {
	code, out, errs := runGen(t, "cc")
	if code != exitUsage || out != "" || !strings.Contains(errs, "pass --out <dir>") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// --out writes each file under the directory and prints the paths written.
func TestGenOutWritesEveryFile(t *testing.T) {
	dir := t.TempDir()
	code, out, errs := runGen(t, "cc", "--out", dir)
	if code != exitVerdict {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, p := range []string{"skills/bops/SKILL.md", "hooks.json", "commands/bops-next.md"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if b, err := os.ReadFile(full); err != nil || len(b) == 0 {
			t.Errorf("%s: %v (%d bytes)", p, err, len(b))
		}
		if !strings.Contains(out, full+"\n") {
			t.Errorf("stdout does not list %s:\n%s", full, out)
		}
	}
	code, _, _ = runGen(t, "docs", "--out="+dir)
	if _, err := os.Stat(filepath.Join(dir, "COMMANDS.md")); code != exitVerdict || err != nil {
		t.Fatalf("gen docs --out=: exit %d, %v", code, err)
	}
}

// A single-file target still prints to stdout, so the CI diff keeps working.
func TestGenSingleFileTargetPrints(t *testing.T) {
	code, out, _ := runGen(t, "docs")
	if code != exitVerdict || !strings.HasPrefix(out, "# bops commands (contract 1)") {
		t.Fatalf("exit %d: %.60q", code, out)
	}
}

func TestGenRefusesUnknownFlags(t *testing.T) {
	for _, args := range [][]string{{"cc", "--outdir", "x"}, {"cc", "--out"}, {"nope"}} {
		if code, _, _ := runGen(t, args...); code != exitUsage {
			t.Errorf("gen %v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
