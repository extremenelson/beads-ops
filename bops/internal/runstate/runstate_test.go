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
