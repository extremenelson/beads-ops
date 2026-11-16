package main

import (
	"os"
	"strings"
	"testing"
)

func TestDepsUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, args := range [][]string{{"deps"}, {"deps", "a-1", "a-2"}, {"deps", "a-1", "--engine", "sql"}} {
		stdout, _ := os.CreateTemp(t.TempDir(), "out")
		stderr, _ := os.CreateTemp(t.TempDir(), "err")
		code := run(args, stdout, stderr)
		e, _ := os.ReadFile(stderr.Name())
		if code != exitUsage || !strings.Contains(string(e), "usage: bops deps <id>") {
			t.Errorf("bops %v: exit %d, stderr %q", args, code, e)
		}
	}
}
