package runstate

import (
	"os"
	"os/exec"
	"path/filepath"
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
	if !strings.Contains(string(b), `"version": 2`) {
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
