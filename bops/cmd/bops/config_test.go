package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// configDir is a non-git directory, so it is its own worktree root, with
// state and user config kept out of the real home.
func configDir(t *testing.T, repoJSON string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, k := range []string{"BOPS_POOL", "BOPS_CAPS", "BOPS_STALE_MINUTES", "BOPS_HOLD_MINUTES", "BOPS_LOCK_REMOTE"} {
		t.Setenv(k, "")
	}
	if repoJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, ".bops.json"), []byte(repoJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewEnvAppliesConfig(t *testing.T) {
	configDir(t, `{"stale_minutes":45,"hold_minutes":30,"pool":"docs","caps":["gpu"]}`)
	t.Setenv("BOPS_POOL", "ops") // the environment still beats the file
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if env.StaleAfter != 45*time.Minute || env.HoldFor != 30*time.Minute {
		t.Errorf("stale=%v hold=%v, want 45m and 30m", env.StaleAfter, env.HoldFor)
	}
	if env.Run.Pool != "ops" || len(env.Run.Caps) != 1 || env.Run.Caps[0] != "gpu" {
		t.Errorf("pool=%q caps=%v", env.Run.Pool, env.Run.Caps)
	}
}

func TestNewEnvDefaultsTo120Minutes(t *testing.T) {
	configDir(t, "")
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if env.StaleAfter != 120*time.Minute || env.HoldFor != 120*time.Minute {
		t.Errorf("stale=%v hold=%v, want 120m each", env.StaleAfter, env.HoldFor)
	}
}

func TestNewEnvAppliesLockRemote(t *testing.T) {
	configDir(t, `{"lock":{"remote":"upstream"}}`)
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if env.Lock.Remote != "upstream" {
		t.Errorf("lock remote = %q, want upstream", env.Lock.Remote)
	}
}

func TestNewEnvRefusesABadConfig(t *testing.T) {
	configDir(t, `{"stale_minutes":"soon"}`)
	_, _, err := newEnv(runstate.Read)
	if err == nil || !strings.Contains(err.Error(), ".bops.json") || !strings.Contains(err.Error(), "stale_minutes") {
		t.Fatalf("err = %v, want it to name .bops.json and stale_minutes", err)
	}
}
