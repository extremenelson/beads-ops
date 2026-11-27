package runstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The record sits beside state.json, leaves its version alone, and is read
// back by a Store and by ReadLedgerConflict, which writes nothing.
func TestLedgerConflictRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	if c, err := ReadLedgerConflict(dir); err != nil || c != nil {
		t.Fatalf("before any record: %+v, %v", c, err)
	}
	s, err := Open(dir, Write)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := &LedgerConflict{At: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), Command: "sync", Ref: "origin/main", Beads: []string{"x-1"}, BeadsRead: true, Detail: "d"}
	if err := s.SetLedgerConflict(want); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (*LedgerConflict, error){s.LedgerConflict, func() (*LedgerConflict, error) { return ReadLedgerConflict(dir) }} {
		got, err := read()
		if err != nil || got == nil || got.Command != "sync" || len(got.Beads) != 1 || !got.BeadsRead || !got.At.Equal(want.At) {
			t.Errorf("got %+v, %v", got, err)
		}
	}
	if st, err := s.load(); err != nil || st.Version != stateVersion {
		t.Errorf("state.json version %d, %v", st.Version, err)
	}
	if err := s.SetLedgerConflict(nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLedgerConflict(nil); err != nil { // removing nothing is fine
		t.Fatal(err)
	}
	if c, _ := s.LedgerConflict(); c != nil {
		t.Errorf("after clearing: %+v", c)
	}
	if left, _ := filepath.Glob(filepath.Join(s.dir, conflictFile+"*")); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "state.json")); err != nil {
		t.Error(err)
	}
}
