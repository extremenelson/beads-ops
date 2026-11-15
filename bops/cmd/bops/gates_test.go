package main

import (
	"os"
	"strings"
	"testing"
)

// gates takes exactly one bead id, and is implemented: it no longer prints
// "not yet implemented".
func TestGatesUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, args := range [][]string{{"gates"}, {"gates", "a-1", "a-2"}} {
		stdout, _ := os.CreateTemp(t.TempDir(), "out")
		stderr, _ := os.CreateTemp(t.TempDir(), "err")
		code := run(args, stdout, stderr)
		e, _ := os.ReadFile(stderr.Name())
		if code != exitUsage || !strings.Contains(string(e), "usage: bops gates <id>") || strings.Contains(string(e), "not yet implemented") {
			t.Errorf("bops %v: exit %d, stderr %q", args, code, e)
		}
	}
}
