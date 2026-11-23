package bdx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// script writes an executable stand-in for bd.
func script(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is POSIX; Windows kills bd as before")
	}
	p := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// When a command's time runs out, bd is asked to stop with SIGTERM before
// anything kills it, so it can stop between steps of a Dolt merge (bops-9rb).
func TestRunSendsSIGTERMWhenTimeRunsOut(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "term")
	bd := script(t, `trap 'echo term > "`+mark+`"; exit 143' TERM
while :; do sleep 0.05; done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := (Exec{Bin: bd}).Run(ctx, "dolt", "pull"); err == nil {
		t.Fatal("a cancelled bd must fail")
	}
	if b, err := os.ReadFile(mark); err != nil || string(b) != "term\n" {
		t.Errorf("bd did not see SIGTERM: %q, %v", b, err)
	}
}

// A bd that ignores SIGTERM is killed once the grace is over.
func TestRunKillsABDThatIgnoresSIGTERM(t *testing.T) {
	old := TermGrace
	TermGrace = 300 * time.Millisecond
	t.Cleanup(func() { TermGrace = old })
	bd := script(t, `trap '' TERM
while :; do sleep 0.05; done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (Exec{Bin: bd}).Run(ctx, "dolt", "pull"); err == nil {
		t.Fatal("a killed bd must fail")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v; want the kill soon after the %v grace", took, TermGrace)
	}
}
