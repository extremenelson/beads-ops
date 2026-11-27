package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// A ledger pull recorded as stuck fails doctor's ledger_conflict check with
// the beads and the runbook. Doctor only reads the record.
func TestDoctorReportsARecordedLedgerConflict(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	code, got := runDoctor(t, func(clone string) {
		s, err := runstate.Open(clone, runstate.Write)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.SetLedgerConflict(&runstate.LedgerConflict{At: time.Now(), Command: "land", Ref: "origin/main", Beads: []string{"x-1"}, BeadsRead: true, Detail: "merge conflicts in issues require operator resolution"}); err != nil {
			t.Fatal(err)
		}
	})
	if code != exitCantRun || got["ok"] != false || got["code"] != lock.CodeLedgerConflict {
		t.Fatalf("exit %d: %v", code, got)
	}
	if failed, _ := got["failed"].([]any); len(failed) != 1 || failed[0] != intent.LedgerConflictCheckID {
		t.Errorf("failed = %v", got["failed"])
	}
	checks, _ := got["checks"].([]any)
	last := checks[len(checks)-1].(map[string]any)
	if last["id"] != intent.LedgerConflictCheckID || !strings.Contains(last["detail"].(string), "x-1") || !strings.Contains(last["next"].(string), "MULTI-DEV-PLAN.md §6") {
		t.Errorf("check = %v", last)
	}
	files, _ := filepath.Glob(filepath.Join(state, "bops", "*", "ledger-conflict.json"))
	if len(files) != 1 {
		t.Errorf("doctor must leave the record: %v", files)
	}
}

// With no record there is no check, and doctor creates no state.
func TestDoctorWithNoLedgerConflictAddsNoCheckAndNoState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	code, got := runDoctor(t, nil)
	if code != exitVerdict {
		t.Fatalf("exit %d: %v", code, got)
	}
	for _, c := range got["checks"].([]any) {
		if c.(map[string]any)["id"] == intent.LedgerConflictCheckID {
			t.Errorf("check = %v", c)
		}
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Errorf("doctor wrote state: %v", entries)
	}
}

// A record doctor cannot read is a warning, not a failed check.
func TestDoctorWarnsOnAnUnreadableLedgerConflict(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	code, got := runDoctor(t, func(clone string) {
		s, err := runstate.Open(clone, runstate.Write)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		dirs, _ := filepath.Glob(filepath.Join(state, "bops", "*"))
		if len(dirs) != 1 {
			t.Fatalf("state dirs %v", dirs)
		}
		if err := os.WriteFile(filepath.Join(dirs[0], "ledger-conflict.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if code != exitVerdict {
		t.Fatalf("exit %d: %v", code, got)
	}
	found := false
	for _, w := range got["warnings"].([]any) {
		found = found || strings.HasPrefix(w.(string), "ledger conflict record not read: ")
	}
	if !found {
		t.Errorf("warnings = %v", got["warnings"])
	}
}
