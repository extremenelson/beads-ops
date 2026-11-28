package runstate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repo creates a clone with one commit and isolates the state directory.
func repo(t *testing.T) (root, stateHome string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateHome = t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("BEADS_ACTOR", "tester")
	git(t, root, "init", "-q")
	git(t, root, "config", "user.email", "t@example.invalid")
	git(t, root, "config", "user.name", "tester")
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	return root, stateHome
}

func worktree(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(root), name)
	git(t, root, "worktree", "add", "-q", path, "-b", name)
	return path
}

func open(t *testing.T, dir string, mode Mode) *Store {
	t.Helper()
	s, err := Open(dir, mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func stateFiles(t *testing.T, stateHome string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(stateHome, "bops", "*", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSubfolderKeepsRunIdentity(t *testing.T) {
	root, _ := repo(t)
	id := open(t, root, Read).Run().ID
	if got := open(t, filepath.Join(root, "sub", "deep"), Read).Run().ID; got != id {
		t.Errorf("subfolder run = %s, want %s", got, id)
	}
}

func TestWorktreeHasOwnRunInTheCloneState(t *testing.T) {
	root, stateHome := repo(t)
	wt := worktree(t, root, "wt")

	main := open(t, root, Read).Run().ID
	other := open(t, wt, Read).Run().ID
	if main == other {
		t.Errorf("worktree shares the root's run id %s", main)
	}
	if again := open(t, filepath.Join(wt, "."), Read).Run().ID; again != other {
		t.Errorf("worktree run changed: %s then %s", other, again)
	}
	if files := stateFiles(t, stateHome); len(files) != 1 {
		t.Errorf("want one state file per clone, got %v", files)
	}
}

func TestHoldsAreVisibleAcrossWorktrees(t *testing.T) {
	root, _ := repo(t)
	wt := worktree(t, root, "wt")

	s := open(t, wt, Write)
	s.Run().Hold("x-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	holds := open(t, root, Read).Holds()
	if len(holds) != 1 || holds[0].ID != "x-1" || holds[0].Stranded {
		t.Fatalf("holds from the root = %+v", holds)
	}
}

func TestRemovedWorktreeHoldIsRecoverable(t *testing.T) {
	root, stateHome := repo(t)
	wt := worktree(t, root, "wt")

	s := open(t, wt, Write)
	s.Run().Hold("x-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	git(t, root, "worktree", "remove", "--force", wt)

	s = open(t, root, Write)
	holds := s.Holds()
	if len(holds) != 1 || holds[0].ID != "x-1" || !holds[0].Stranded {
		t.Fatalf("want one stranded hold, got %+v", holds)
	}
	s.DropHold("x-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if holds := open(t, root, Read).Holds(); len(holds) != 0 {
		t.Errorf("hold should be gone: %+v", holds)
	}
	b, err := os.ReadFile(stateFiles(t, stateHome)[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), wt) {
		t.Errorf("removed worktree should be pruned from the state file:\n%s", b)
	}
}

func TestStrandedHoldSurvivesSaveUntilDropped(t *testing.T) {
	root, _ := repo(t)
	wt := worktree(t, root, "wt")
	s := open(t, wt, Write)
	s.Run().Hold("x-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	git(t, root, "worktree", "remove", "--force", wt)

	s = open(t, root, Write)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if holds := open(t, root, Read).Holds(); len(holds) != 1 {
		t.Errorf("Save must keep a stranded hold: %+v", holds)
	}
}

func shortLockWait(t *testing.T) {
	t.Helper()
	old := lockWait
	lockWait = 100 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
}

func TestWriteLockIsPerClone(t *testing.T) {
	root, _ := repo(t)
	wt := worktree(t, root, "wt")
	shortLockWait(t)

	first := open(t, root, Write)
	if _, err := Open(wt, Write); err == nil || !strings.Contains(err.Error(), "state lock") {
		t.Fatalf("a second writer in another worktree should wait and fail, got %v", err)
	}
	first.Close()
	open(t, wt, Write)
}

func TestReadDoesNotWaitForAWriter(t *testing.T) {
	root, _ := repo(t)
	id := open(t, root, Read).Run().ID
	shortLockWait(t)
	open(t, root, Write)

	s, err := Open(root, Read)
	if err != nil {
		t.Fatalf("read blocked by a writer: %v", err)
	}
	if s.Run().ID != id {
		t.Errorf("run = %s, want %s", s.Run().ID, id)
	}
}

func TestAbandonedLockIsBroken(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Write)
	lock := filepath.Join(s.dir, "lock")
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	s.locked = false // simulate the crashed owner, which never removes it

	shortLockWait(t)
	open(t, root, Write)
}

func TestLegacyHeldListStillLoads(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	dir := filepath.Dir(firstOrEmpty(stateFiles(t, stateHome)))
	legacy := `{"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":["x-1","x-2"]}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	w := open(t, root, Write)
	if got := w.Run().HeldIDs(); len(got) != 2 || got[0] != "x-1" || got[1] != "x-2" {
		t.Fatalf("held = %v", got)
	}
	e, ok := w.Entry("x-1")
	if !ok || !e.Legacy() {
		t.Errorf("entry = %+v, %v", e, ok)
	}
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	w.Close()

	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 6`) {
		t.Errorf("not migrated:\n%s", b)
	}
	if got := open(t, root, Read).Run().HeldIDs(); len(got) != 2 {
		t.Errorf("after migration: %v", got)
	}
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func TestNewerStateFileIsRefused(t *testing.T) {
	root, stateHome := repo(t)
	open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	if err := os.WriteFile(path, []byte(`{"version":99,"runs":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, Read); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloseLeavesAnotherProcessesLock(t *testing.T) {
	root, _ := repo(t)
	first := open(t, root, Write)
	lock := filepath.Join(first.dir, "lock")
	if err := os.WriteFile(lock, []byte("someone else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("another process's lock was removed: %v", err)
	}
}

// The journal must not need the lock between its own writes, or a command
// would hold it across a push.
func TestJournalHoldsNoLockBetweenWrites(t *testing.T) {
	root, _ := repo(t)
	shortLockWait(t)
	s := open(t, root, Read)
	if err := s.Intend("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	other := open(t, root, Write) // would fail if Intend had kept the lock
	other.Close()

	if e, _ := s.Entry("x-1"); e.Phase != PhasePushing || e.Intended != "aaa" {
		t.Errorf("after intend: %+v", e)
	}
	if err := s.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.Intend("x-1", "bbb"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Entry("x-1"); e.Phase != PhaseHeld || e.SHA != "aaa" || e.Intended != "bbb" {
		t.Errorf("a refresh must keep the claim: %+v", e)
	}
	if err := s.Abandon("x-1"); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Entry("x-1"); !ok || e.Intended != "" || e.SHA != "aaa" {
		t.Errorf("after abandon: %+v, %v", e, ok)
	}

	again := open(t, root, Read)
	if e, ok := again.Entry("x-1"); !ok || e.SHA != "aaa" {
		t.Errorf("not persisted: %+v, %v", e, ok)
	}
}

func TestAbandonRemovesAnUnconfirmedEntry(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	if err := s.Intend("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon("x-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Entry("x-1"); ok {
		t.Error("a write that never landed leaves nothing")
	}
}

func TestJournalWritesDoNotLoseOtherWorktrees(t *testing.T) {
	root, _ := repo(t)
	wt := worktree(t, root, "wt")
	a, b := open(t, root, Read), open(t, wt, Read)
	if err := a.Intend("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := b.Intend("x-2", "bbb"); err != nil { // b read the file before a wrote
		t.Fatal(err)
	}
	if err := a.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if holds := open(t, root, Read).Holds(); len(holds) != 2 {
		t.Errorf("a write dropped another worktree's hold: %+v", holds)
	}
}

func TestStrandedHoldIsFinishedFromAnotherWorktree(t *testing.T) {
	root, _ := repo(t)
	wt := worktree(t, root, "wt")
	s := open(t, wt, Read)
	if err := s.Intend("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	git(t, root, "worktree", "remove", "--force", wt)

	m := open(t, root, Read)
	if e, ok := m.Entry("x-1"); !ok || e.SHA != "aaa" {
		t.Fatalf("entry = %+v, %v", e, ok)
	}
	if err := m.IntendDelete("x-1"); err != nil {
		t.Fatal(err)
	}
	if holds := open(t, root, Read).Holds(); len(holds) != 1 || !holds[0].Entry.DeletePending || holds[0].Run == m.Run().ID {
		t.Fatalf("holds = %+v", holds)
	}
	if err := m.Drop("x-1"); err != nil {
		t.Fatal(err)
	}
	if holds := open(t, root, Read).Holds(); len(holds) != 0 {
		t.Errorf("holds = %+v", holds)
	}
}

func TestBeadsActorOverridesTheSavedActor(t *testing.T) {
	root, _ := repo(t)
	open(t, root, Read)
	t.Setenv("BEADS_ACTOR", "someone-else")
	s := open(t, root, Read)
	if s.Run().Actor != "someone-else" {
		t.Fatalf("actor = %q", s.Run().Actor)
	}
	if err := s.Intend("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if s.Run().Actor != "someone-else" {
		t.Errorf("a journal write reverted the actor to %q", s.Run().Actor)
	}
}

func TestSaveNeedsWriteMode(t *testing.T) {
	root, _ := repo(t)
	if err := open(t, root, Read).Save(); err == nil {
		t.Error("Save on a Read store should fail")
	}
}

func TestOutsideGitUsesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	id := open(t, dir, Read).Run().ID
	if got := open(t, dir, Read).Run().ID; got != id {
		t.Errorf("run = %s, want %s", got, id)
	}
}

// A bead's lock is held by one open at a time, and released when its holder
// lets go (bops-yph).
func TestLockBeadIsExclusive(t *testing.T) {
	root, _ := repo(t)
	a, b := open(t, root, Read), open(t, root, Read)
	unlock, err := a.LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.LockBead("x-1"); !errors.Is(err, ErrBeadBusy) {
		t.Fatalf("a held bead is busy, got %v", err)
	}
	other, err := b.LockBead("x-2")
	if err != nil {
		t.Fatalf("another bead is free: %v", err)
	}
	other()
	unlock()
	again, err := b.LockBead("x-1")
	if err != nil {
		t.Fatalf("a released bead is free: %v", err)
	}
	again()
	if _, err := a.LockBead(""); err == nil {
		t.Error("an empty id must not name a lock file")
	}
	// Every id names a plain file inside beads/: no path, no device name, and
	// ids differing only in case are separate locks on a case-insensitive disk.
	for _, id := range []string{"..", "a/b", `a\b`, "nul", "con", "X-1", "x-1"} {
		release, err := a.LockBead(id)
		if err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		defer release()
	}
	entries, err := os.ReadDir(filepath.Join(a.dir, "beads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 8 { // x-1 and x-2 above, and six more ids here
		t.Errorf("want one lock file per id, got %d", len(entries))
	}
	for _, e := range entries {
		if name := strings.TrimSuffix(e.Name(), ".lock"); len(name) != 32 || strings.ContainsAny(name, "./\\") {
			t.Errorf("lock file %q is not a 32-hex-digit name", e.Name())
		}
	}
}

// The operating system releases a bead's lock when its holder dies, so a
// crashed bops leaves nothing to clean up.
func TestLockBeadIsReleasedWhenItsHolderDies(t *testing.T) {
	if os.Getenv("BOPS_TEST_HOLD_BEAD") != "" {
		s, err := Open(os.Getenv("BOPS_TEST_HOLD_BEAD"), Read)
		if err != nil {
			os.Exit(2)
		}
		if _, err := s.LockBead("x-1"); err != nil {
			os.Exit(3)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	root, _ := repo(t)
	child := exec.Command(os.Args[0], "-test.run=^TestLockBeadIsReleasedWhenItsHolderDies$")
	child.Env = append(os.Environ(), "BOPS_TEST_HOLD_BEAD="+root)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	s := open(t, root, Read)
	deadline := time.Now().Add(10 * time.Second)
	for {
		unlock, err := s.LockBead("x-1")
		if errors.Is(err, ErrBeadBusy) {
			break // the child holds it
		}
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		if time.Now().After(deadline) {
			t.Fatal("the child never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	unlock, err := s.LockBead("x-1")
	if err != nil {
		t.Fatalf("a dead holder's lock is free: %v", err)
	}
	unlock()
}

// Matches reads one run's entry as the file holds it now, and writes nothing.
func TestMatchesReadsOneRunWithoutWriting(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	if err := s.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	w := open(t, worktree(t, root, "wt-matches"), Write) // another run holding it differently
	w.Run().Held = Held{"x-1": {SHA: "zzz", Phase: PhaseHeld}}
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	files := stateFiles(t, stateHome)
	before, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.Refresh(); err != nil { // the caller re-reads under the bead lock
		t.Fatal(err)
	}
	held := Entry{SHA: "aaa", Phase: PhaseHeld}
	if !s.Matches(s.Run().ID, "x-1", held) {
		t.Fatal("its own run's entry matches whatever another run holds")
	}
	if s.Matches(s.Run().ID, "x-1", Entry{SHA: "bbb", Phase: PhaseHeld}) {
		t.Fatal("a different entry does not match")
	}
	after, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("Matches rewrote the state file")
	}
}

// A write confirmed over a delete that never finished is the claim now:
// sync must not go on to delete it.
func TestConfirmClearsADeleteThatNeverFinished(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	if err := s.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.IntendDelete("x-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm("x-1", "bbb"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Entry("x-1"); e.DeletePending || e.SHA != "bbb" {
		t.Fatalf("entry = %+v", e)
	}
}

// A release waiting on its ledger push survives a reopen, and a confirmed
// write (a claim again) clears it, as it clears a pending delete.
func TestReleasePendingPersistsAndConfirmClearsIt(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	if err := s.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReleasePending("x-1", true); err != nil {
		t.Fatal(err)
	}
	if e, _ := open(t, root, Read).Entry("x-1"); !e.ReleasePending || e.SHA != "aaa" {
		t.Fatalf("reopened entry = %+v", e)
	}
	if err := s.SetReleasePending("x-none", true); err == nil {
		t.Error("marked a bead no run holds release pending, which nothing would finish")
	}
	if _, ok := s.Entry("x-none"); ok {
		t.Error("SetReleasePending made an entry for a bead not held")
	}
	if err := s.Confirm("x-1", "bbb"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Entry("x-1"); e.ReleasePending || e.SHA != "bbb" {
		t.Fatalf("entry = %+v", e)
	}
}

// A done release marks its hold release pending and its display owed in one
// write, so no crash leaves one flag without the other.
func TestSetReleaseOwedSetsBothFlagsAtOnce(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	if err := s.Confirm("x-1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReleaseOwed("x-1"); err != nil {
		t.Fatal(err)
	}
	if e, _ := open(t, root, Read).Entry("x-1"); !e.ReleasePending || !e.LedgerPending || !e.Given() || e.SHA != "aaa" {
		t.Fatalf("reopened entry = %+v", e)
	}
	if err := s.SetReleaseOwed("x-none"); err == nil {
		t.Error("marked a bead no run holds, which nothing would finish")
	}
	if _, ok := s.Entry("x-none"); ok {
		t.Error("SetReleaseOwed made an entry for a bead not held")
	}
}

// Every id the claim contract accepts names a lock file, the longest too.
func TestLockBeadTakesTheLongestValidID(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	release, err := s.LockBead(strings.Repeat("a", 128))
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// A newer bops stamps its version on the state file as soon as it opens it,
// read-only commands included, so an older bops that takes no bead lock
// refuses the clone from then on rather than only after the newer one first
// writes (bops-j9k). Every run's holds come through, and a file already at
// this version is not rewritten.
func TestOpenStampsAnOlderStateFile(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{` +
		`"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":{"x-1":{"sha":"aaa","phase":"held"}}},` +
		`"/elsewhere":{"id":"SIB","machine":"m","actor":"b","started_at":"2026-11-09T00:00:00Z","held":{"x-2":{"sha":"bbb","phase":"held"}}}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}

	r := open(t, root, Read)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 6`) {
		t.Fatalf("opening must stamp the version:\n%s", b)
	}
	for id, sha := range map[string]string{"x-1": "aaa", "x-2": "bbb"} {
		if e, ok := r.Entry(id); !ok || e.SHA != sha {
			t.Fatalf("stamping must keep every run's holds: %s = %+v, %v\n%s", id, e, ok, b)
		}
	}

	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	w := open(t, root, Write)
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"version": 6`) {
		t.Fatalf("a write open must stamp the version too:\n%s", b)
	}
	w.Close()

	// write always indents, so a rewrite of this compact file would show.
	current := `{"version":6,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	open(t, root, Read)
	if b, err := os.ReadFile(path); err != nil || string(b) != current {
		t.Fatalf("a file already at this version must not be rewritten: %v\n%s", err, b)
	}
}

// Stamping is best-effort for a read-only command: one that cannot take the
// state lock still reads the file, and a later command stamps it (bops-j9k).
// The other process's lock is left alone.
func TestReadOpenSkipsTheStampWhenTheLockIsBusy(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":{"x-1":{"sha":"aaa","phase":"held"}}}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(filepath.Dir(path), "lock")
	if err := os.WriteFile(lock, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Under the full wait: the stamp must not spend a command's budget on a
	// lock another process (a crashed one, say) holds.
	start := time.Now()
	r, err := Open(root, Read)
	if took := time.Since(start); took > lockWait/2 { // gstack-timing-ok: bound is half the 30 s wait the stamp must not take
		t.Fatalf("a read waited %v for the stamp's lock", took)
	}
	if err != nil {
		t.Fatalf("a read must not fail for want of the stamp: %v", err)
	}
	if e, ok := r.Entry("x-1"); !ok || e.SHA != "aaa" {
		t.Fatalf("the read must still see the file: %+v, %v", e, ok)
	}
	if b, _ := os.ReadFile(path); string(b) != older {
		t.Fatalf("the file must be left for a later command to stamp:\n%s", b)
	}
	if b, _ := os.ReadFile(lock); string(b) != "other\n" {
		t.Fatalf("another process's lock must be left alone: %q", b)
	}
}

// A read-only command in a state directory it cannot write still reads, and
// leaves the file for a later command to stamp (bops-j9k).
func TestReadOpenSkipsTheStampWhenTheDirIsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind the caller")
	}
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":{"x-1":{"sha":"aaa","phase":"held"}}}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	r, err := Open(root, Read)
	if err != nil {
		t.Fatalf("a read must not fail for want of the stamp: %v", err)
	}
	if e, ok := r.Entry("x-1"); !ok || e.SHA != "aaa" {
		t.Fatalf("the read must still see the file: %+v, %v", e, ok)
	}
	if b, _ := os.ReadFile(path); string(b) != older {
		t.Fatalf("the file must be left for a later command to stamp:\n%s", b)
	}
}

// The stamp writes the run's stored actor, not this process's BEADS_ACTOR,
// which names who is acting now and is never saved by a read.
func TestStampKeepsTheStoredActor(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"stored","started_at":"2026-11-09T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_ACTOR", "someone-else")

	r := open(t, root, Read)
	if r.run.Actor != "someone-else" {
		t.Fatalf("this process acts as BEADS_ACTOR: %q", r.run.Actor)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 6`) || !strings.Contains(string(b), `"actor": "stored"`) || strings.Contains(string(b), "someone-else") {
		t.Fatalf("the stamp must keep the stored actor:\n%s", b)
	}
}

// A read-only stamp whose lock it took but whose write fails still reads,
// leaves the file as it was and lets go of the lock (bops-mx0).
func TestReadOpenSkipsTheStampWhenTheWriteFails(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":{"x-1":{"sha":"aaa","phase":"held"}}}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil { // write's temp file cannot be created
		t.Fatal(err)
	}

	r, err := Open(root, Read)
	if err != nil {
		t.Fatalf("a read must not fail for want of the stamp: %v", err)
	}
	if e, ok := r.Entry("x-1"); !ok || e.SHA != "aaa" {
		t.Fatalf("the read must still see the file: %+v, %v", e, ok)
	}
	if b, _ := os.ReadFile(path); string(b) != older {
		t.Fatalf("the file must be left for a later command to stamp:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "lock")); !os.IsNotExist(err) {
		t.Fatalf("the stamp must let go of the lock: %v", err)
	}
}

// The stamp's re-read under the lock still refuses a file a newer bops wrote
// since Open's first read, and lets go of the lock (bops-mx0).
func TestStampRefusesANewerFileOnReRead(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	if err := os.WriteFile(path, []byte(`{"version":99,"runs":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.st.Version = 2 // as Open's first read found it

	if err := s.stamp(Read); err == nil || !strings.Contains(err.Error(), "upgrade it") {
		t.Fatalf("a newer file must be refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "lock")); !os.IsNotExist(err) {
		t.Fatalf("the stamp must let go of the lock: %v", err)
	}
}

// A state file gone by the stamp's re-read is not recreated, empty, by a
// read-only command (bops-mx0).
func TestStampDoesNotRecreateAVanishedFile(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.st.Version = 2
	run := s.run

	if err := s.stamp(Read); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		b, _ := os.ReadFile(path)
		t.Fatalf("the stamp must not write a file it no longer finds:\n%s", b)
	}
	if s.run != run {
		t.Fatal("the store must keep handing out its run")
	}
}

// A stamp whose re-read lacks this worktree's run keeps the run it hands out
// and does not write it back; the next write that needs it does (bops-mx0).
func TestStampKeepsItsRunWhenTheReReadLacksIt(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{"/elsewhere":{"id":"SIB","machine":"m","actor":"b","started_at":"2026-11-09T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	s.st.Version = 2
	run := s.run

	if err := s.stamp(Read); err != nil {
		t.Fatal(err)
	}
	if s.run != run {
		t.Fatal("the store must keep handing out its run")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"version": 6`) || !strings.Contains(string(b), `"SIB"`) || strings.Contains(string(b), run.ID) {
		t.Fatalf("the stamp must write only what it re-read:\n%s", b)
	}
}

// A run with no stored actor is not given this process's actor by a read or
// a stamp; only the run this process hands out gets a default, in memory
// (bops-mx0).
func TestStampLeavesASiblingsEmptyActor(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	older := `{"version":2,"runs":{` +
		`"` + s.worktree + `":{"id":"RUN","machine":"m","started_at":"2026-11-09T00:00:00Z"},` +
		`"/elsewhere":{"id":"SIB","machine":"m","started_at":"2026-11-09T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_ACTOR", "someone-else")

	r := open(t, root, Read)
	if r.run.Actor != "someone-else" {
		t.Fatalf("this process acts as BEADS_ACTOR: %q", r.run.Actor)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 6`) || strings.Contains(string(b), "someone-else") {
		t.Fatalf("the stamp must not save this process's actor on any run:\n%s", b)
	}
}

// With no BEADS_ACTOR, a run recorded without an actor is handed out as the
// git user, and only that run is saved with it; a sibling's stays as stored
// (bops-mx0). A claim ref must never carry an empty actor.
func TestOpenDefaultsOnlyItsOwnEmptyActor(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	stored := `{"version":6,"runs":{` +
		`"` + s.worktree + `":{"id":"RUN","machine":"m","started_at":"2026-11-09T00:00:00Z"},` +
		`"/elsewhere":{"id":"SIB","machine":"m","started_at":"2026-11-09T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_ACTOR", "")
	want := Actor()
	if want == "" {
		t.Fatal("Actor() must never be empty")
	}

	w := open(t, root, Write)
	if w.Run().Actor != want {
		t.Fatalf("this run must default to Actor() %q: %q", want, w.Run().Actor)
	}
	if err := w.write(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), `"actor": `+strconv.Quote(want)) != 1 {
		t.Fatalf("only this run may be saved with the default actor:\n%s", b)
	}
}

// Two processes that both find events.jsonl full must not both rotate it:
// the second would move the first's fresh file over events.jsonl.1 and lose
// the old copy. Rotation re-checks the size under its own lock (bops-qef).
func TestLogRotationKeepsTheOldCopyWhenTwoRotateAtOnce(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	path := filepath.Join(s.dir, "events.jsonl")
	old := strings.Repeat(`{"step":"OLD"}`+"\n", eventsMax/15+1)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// The first Log has seen the file full; before it rotates, another
	// process rotates and appends.
	raced := false
	beforeRotate = func() {
		if raced {
			return
		}
		raced = true
		s.Log(Event{Step: "OTHER"})
	}
	t.Cleanup(func() { beforeRotate = func() {} })

	s.Log(Event{Step: "MINE"})

	b, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"OLD"`) {
		t.Fatalf("the old copy was lost; events.jsonl.1 holds %d bytes: %.200s", len(b), b)
	}
	cur, _ := os.ReadFile(path)
	if !strings.Contains(string(cur), "OTHER") || !strings.Contains(string(cur), "MINE") {
		t.Fatalf("both new lines belong in events.jsonl:\n%s", cur)
	}
}

// A rotation another process is in the middle of is left to it: Log appends
// and does not wait (bops-qef).
func TestLogSkipsARotationAnotherProcessHolds(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	path := filepath.Join(s.dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", eventsMax)), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := lockFile(filepath.Join(s.dir, "events.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	s.Log(Event{Step: "MINE"})
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a rotation another process holds must be left to it: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "MINE") {
		t.Fatal("the line must still be appended")
	}
}

// A rotation lock that cannot be taken for any reason but another holder
// (here a directory where events.lock should be) still rotates, unserialized
// as before, rather than let events.jsonl grow without end (bops-qef).
func TestLogRotatesWhenItsLockCannotBeTaken(t *testing.T) {
	root, _ := repo(t)
	s := open(t, root, Read)
	path := filepath.Join(s.dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", eventsMax)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.dir, "events.lock"), 0o700); err != nil {
		t.Fatal(err)
	}

	s.Log(Event{Step: "MINE"})
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("a lock that cannot be taken must not stop rotation: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "MINE") || len(b) > 1000 {
		t.Fatalf("the line belongs in a fresh events.jsonl: %d bytes", len(b))
	}
}

// BOPS_RUN gives a command a run of its own in a worktree, apart from the
// worktree's run, and the same name finds the same run again (bops-4ez).
func TestNamedRunIsApartFromItsWorktreesRun(t *testing.T) {
	root, _ := repo(t)
	own := open(t, root, Write)
	own.Run().Hold("hand-1")
	if err := own.Save(); err != nil {
		t.Fatal(err)
	}
	own.Close()

	t.Setenv("BOPS_RUN", "bead-loop")
	named := open(t, root, Write)
	if named.Run().ID == own.Run().ID || named.Run().Name != "bead-loop" {
		t.Fatalf("BOPS_RUN must name a run of its own: %+v, worktree's is %s", named.Run(), own.Run().ID)
	}
	if named.Worktree() != own.Worktree() {
		t.Fatalf("a named run belongs to its worktree: %s, want %s", named.Worktree(), own.Worktree())
	}
	if _, ok := named.Run().Held["hand-1"]; ok {
		t.Fatal("a named run must not hold the worktree run's beads")
	}
	named.Run().Hold("loop-1")
	if err := named.Save(); err != nil {
		t.Fatal(err)
	}
	named.Close()

	if again := open(t, root, Read); again.Run().ID != named.Run().ID {
		t.Fatalf("the same BOPS_RUN must find the same run: %s, want %s", again.Run().ID, named.Run().ID)
	}
	t.Setenv("BOPS_RUN", "")
	s := open(t, root, Read)
	if s.Run().ID != own.Run().ID {
		t.Fatalf("unset, a command is the worktree's run: %s, want %s", s.Run().ID, own.Run().ID)
	}
	runs := map[string]string{}
	for _, h := range s.Holds() {
		if h.Stranded {
			t.Errorf("a live worktree's named run is not stranded: %+v", h)
		}
		runs[h.ID] = h.Run
	}
	if runs["hand-1"] != own.Run().ID || runs["loop-1"] != named.Run().ID {
		t.Fatalf("holds = %v", runs)
	}
}

func TestBadRunNameIsRefused(t *testing.T) {
	root, _ := repo(t)
	for _, bad := range []string{"-x", "a/b", "a b", "#run=x", strings.Repeat("a", 65)} {
		t.Setenv("BOPS_RUN", bad)
		if s, err := Open(root, Read); err == nil {
			s.Close()
			t.Errorf("BOPS_RUN %q was accepted", bad)
		}
	}
}

// A named run whose worktree is removed is stranded like any other, and
// pruned once it holds nothing.
func TestNamedRunOfARemovedWorktreeIsStranded(t *testing.T) {
	root, stateHome := repo(t)
	wt := worktree(t, root, "wt")
	t.Setenv("BOPS_RUN", "loop")
	s := open(t, wt, Write)
	s.Run().Hold("x-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	git(t, root, "worktree", "remove", "--force", wt)

	t.Setenv("BOPS_RUN", "")
	s = open(t, root, Write)
	if holds := s.Holds(); len(holds) != 1 || !holds[0].Stranded {
		t.Fatalf("want one stranded hold, got %+v", holds)
	}
	s.DropHold("x-1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	b, err := os.ReadFile(stateFiles(t, stateHome)[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "#run=loop") {
		t.Errorf("an empty named run of a removed worktree must be pruned:\n%s", b)
	}
}

// A version 4 file, the last before named runs, loads with its holds and is
// stamped at the current version, which a bops that knows no named run refuses.
func TestVersionFourStateLoadsAndIsStamped(t *testing.T) {
	root, stateHome := repo(t)
	s := open(t, root, Read)
	path := firstOrEmpty(stateFiles(t, stateHome))
	v4 := `{"version":4,"runs":{"` + s.worktree + `":{"id":"RUN","machine":"m","actor":"a","started_at":"2026-11-09T00:00:00Z","held":{"x-1":{"sha":"aaa","phase":"held"}}}}}`
	if err := os.WriteFile(path, []byte(v4), 0o600); err != nil {
		t.Fatal(err)
	}
	w := open(t, root, Write)
	if w.Run().ID != "RUN" {
		t.Fatalf("the worktree's run must be the stored one: %+v", w.Run())
	}
	if e, ok := w.Entry("x-1"); !ok || e.SHA != "aaa" {
		t.Fatalf("hold lost: %+v %v", e, ok)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"version": 6`) {
		t.Fatalf("not stamped:\n%s", b)
	}
}
