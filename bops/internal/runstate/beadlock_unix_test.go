//go:build !windows

package runstate

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// flock's busy answers are ErrBeadBusy, and a file system with no locks is
// ErrBeadLockUnsupported, so a command can run unserialized there and warn
// rather than fail (bops-yph).
func TestLockErrorNamesBusyAndUnsupported(t *testing.T) {
	// A slice, not a map: on Linux ENOTSUP and EOPNOTSUPP are one value.
	for _, c := range []struct{ err, want error }{
		{syscall.EWOULDBLOCK, ErrBeadBusy},
		{syscall.ENOTSUP, ErrBeadLockUnsupported},
		{syscall.EOPNOTSUPP, ErrBeadLockUnsupported},
		{syscall.ENOLCK, ErrBeadLockUnsupported},
	} {
		if got := lockError(c.err); !errors.Is(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.err, got, c.want)
		}
	}
	if got := lockError(syscall.EIO); errors.Is(got, ErrBeadBusy) || errors.Is(got, ErrBeadLockUnsupported) {
		t.Errorf("EIO must stay itself, got %v", got)
	}
}

// A lock file pruned between LockBead opening it and locking it would leave
// LockBead holding a file no path names, beside a fresh one another process
// can lock. LockBead re-checks after locking that the path still names its
// file, and takes the new one if not (bops-exb).
func TestLockBeadRetakesAFilePrunedBeforeItLocked(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	path := beadLockPath(s.dir, "x-1")
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pruned := false
	beforeFlock = func() {
		if !pruned {
			pruned = true
			_ = os.Remove(path)
		}
	}
	t.Cleanup(func() { beforeFlock = func() {} })

	release, err := s.LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !pruned {
		t.Fatal("the hook must have run")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("LockBead must hold a file the path names: %v", err)
	}
	if _, err := s.LockBead("x-1"); !errors.Is(err, ErrBeadBusy) {
		t.Fatalf("the file at the path must be the one held: %v", err)
	}
}

// PruneBeadLocks removes the lock files of beads no run holds and that no
// process has locked, and keeps the rest (bops-exb).
func TestPruneBeadLocksRemovesOnlyFreeUnheldFiles(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	held := `{"version":4,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":{"held-1":{"sha":"aaa","phase":"held"}}},` +
		`"/elsewhere":{"id":"SIB","machine":"m","actor":"b","started_at":"2026-11-09T00:00:00Z","held":{"held-2":{"sha":"bbb","phase":"held"}}}}}`
	if err := os.WriteFile(path, []byte(held), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"held-1", "held-2", "free-1", "free-2", "busy-1"} {
		release, err := s.LockBead(id)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	busy, err := s.LockBead("busy-1")
	if err != nil {
		t.Fatal(err)
	}
	defer busy()
	other := filepath.Join(s.dir, "beads", "notes.txt") // not a lock file bops names
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneBeadLocks()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d files, want the 2 free unheld ones", n)
	}
	for id, want := range map[string]bool{"held-1": true, "held-2": true, "free-1": false, "free-2": false, "busy-1": true} {
		_, err := os.Stat(beadLockPath(s.dir, id))
		if got := err == nil; got != want {
			t.Errorf("%s: file present = %v, want %v", id, got, want)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a file bops did not name must be left: %v", err)
	}
	if _, err := s.LockBead("busy-1"); !errors.Is(err, ErrBeadBusy) {
		t.Errorf("a held lock must stay held through a prune: %v", err)
	}
}

// A prune removes nothing until the state file on disk carries this version:
// bops 0.4.0 to 0.4.3 accepts an older file and does not re-check after locking, so
// it could lock a pruned file while a newer bops locks its replacement. A
// read-only stamp can be skipped, so the prune checks the file itself
// (bops-exb).
func TestPruneBeadLocksNeedsAStampedFile(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	release, err := s.LockBead("free-1")
	if err != nil {
		t.Fatal(err)
	}
	release()
	older := `{"version":3,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneBeadLocks()
	if err != nil || n != 0 {
		t.Fatalf("an unstamped file must prune nothing: %d, %v", n, err)
	}
	if _, err := os.Stat(beadLockPath(s.dir, "free-1")); err != nil {
		t.Fatalf("the lock file must be left: %v", err)
	}
}

// A file that keeps being replaced between open and flock is given up on as
// busy after lockFileTries attempts, rather than looping (bops-exb).
func TestLockBeadGivesUpWhenItsFileKeepsBeingPruned(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	path := beadLockPath(s.dir, "x-1")
	calls := 0
	beforeFlock = func() { calls++; _ = os.Remove(path) }
	t.Cleanup(func() { beforeFlock = func() {} })

	if _, err := s.LockBead("x-1"); !errors.Is(err, ErrBeadBusy) {
		t.Fatalf("want ErrBeadBusy, got %v", err)
	}
	if calls != lockFileTries {
		t.Fatalf("tried %d times, want %d", calls, lockFileTries)
	}
}

// A re-check that cannot stat the path reports why, rather than retrying and
// calling the bead busy (bops-exb).
func TestLockBeadReportsAReCheckItCannotMake(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory it has no permission on")
	}
	root, _ := repo(t)
	s := open(t, root, Read)
	dir := filepath.Dir(beadLockPath(s.dir, "x-1"))
	beforeFlock = func() { _ = os.Chmod(dir, 0o000) }
	t.Cleanup(func() { beforeFlock = func() {}; _ = os.Chmod(dir, 0o700) })

	_, err := s.LockBead("x-1")
	if err == nil || errors.Is(err, ErrBeadBusy) {
		t.Fatalf("a failed re-check must be its own error, got %v", err)
	}
}

// A prune keeps the lock file of a bead this process holds but has not yet
// written to the state file (bops-exb).
func TestPruneBeadLocksKeepsAnUnsavedOwnHold(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	if err := os.WriteFile(path, []byte(`{"version":4,"runs":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := s.LockBead("mine-1")
	if err != nil {
		t.Fatal(err)
	}
	release()
	s.Run().Hold("mine-1")

	if n, err := s.PruneBeadLocks(); err != nil || n != 0 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if _, err := os.Stat(beadLockPath(s.dir, "mine-1")); err != nil {
		t.Fatalf("this process's unsaved hold lost its lock file: %v", err)
	}
}

// A prune that finds its path now names another file, one a LockBead created
// after the prune opened the old, leaves the new file alone (bops-exb).
func TestPruneLockFileLeavesAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path) // keeps the old file's inode from being reused
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	pruneOpened = func() {
		_ = os.Remove(path)
		_ = os.WriteFile(path, nil, 0o600)
	}
	t.Cleanup(func() { pruneOpened = func() {} })

	removed, err := pruneLockFile(path)
	if err != nil || removed {
		t.Fatalf("removed = %v, %v; want false, nil", removed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the new file must be left: %v", err)
	}
}
