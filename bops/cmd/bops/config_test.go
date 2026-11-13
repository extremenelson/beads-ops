package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/config"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
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
	for _, k := range []string{"BOPS_POOL", "BOPS_CAPS", "BOPS_STALE_MINUTES", "BOPS_HOLD_MINUTES", "BOPS_LOCK_MODE", "BOPS_LOCK_REMOTE"} {
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

func TestNewEnvTurnsTheLockOff(t *testing.T) {
	configDir(t, `{"lock":{"mode":"none"}}`)
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !env.LockOff {
		t.Error("lock.mode none in .bops.json did not turn the lock off")
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

// forgeRunner answers get-url for the named remotes and logs every call.
type forgeRunner struct {
	urls  map[string]string
	calls []string
}

func (r *forgeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "git" && len(args) == 3 && args[0] == "remote" && args[1] == "get-url" {
		if u, ok := r.urls[args[2]]; ok {
			return []byte(u + "\n"), nil
		}
	}
	return nil, errors.New("exit 1")
}

func forgeFlags(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.String("forge", "auto", "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestSelectForgeHonoursConfigWithoutTheFlag(t *testing.T) {
	r := &forgeRunner{urls: map[string]string{"origin": "https://github.com/o/r.git"}}
	cfg := config.Default()
	cfg.Forge = "none"
	f, err := selectForge(context.Background(), forgeFlags(t), cfg, r.run)
	if err != nil || f.Name() != "none" {
		t.Fatalf("got %v, %v; want none from config", f, err)
	}
	f, err = selectForge(context.Background(), forgeFlags(t, "--forge", "gitlab"), cfg, r.run)
	if err != nil || f.Name() != "gitlab" {
		t.Fatalf("got %v, %v; want gitlab from the flag over config", f, err)
	}
	f, err = selectForge(context.Background(), forgeFlags(t, "--forge", "auto"), cfg, r.run)
	if err != nil || f.Name() != "github" {
		t.Fatalf("got %v, %v; want an explicit --forge auto to infer github over config", f, err)
	}
}

func TestSelectForgeInfersFromTheLockRemote(t *testing.T) {
	r := &forgeRunner{urls: map[string]string{
		"origin":   "https://github.com/o/r.git",
		"upstream": "git@gitlab.com:g/p.git",
	}}
	cfg := config.Default()
	cfg.LockRemote = "upstream"
	f, err := selectForge(context.Background(), forgeFlags(t), cfg, r.run)
	if err != nil || f.Name() != "gitlab" {
		t.Fatalf("got %v, %v; want gitlab inferred from upstream", f, err)
	}
	if len(r.calls) != 1 || r.calls[0] != "git remote get-url upstream" {
		t.Errorf("calls = %v, want one get-url of upstream", r.calls)
	}
}

// A bad config is a failure to run, and every such failure carries a stable
// code (SPEC §4), whichever of the three config loads it hits: newEnv (next),
// loadConfig (sweep) or doctor's own.
func TestBadConfigCarriesItsCode(t *testing.T) {
	cases := []struct {
		name, repo, env, user, wantIn string
	}{
		{name: "unknown key", repo: `{"stale_minute":60}`, wantIn: `unknown key "stale_minute"`},
		{name: "out of range", env: "0", wantIn: "BOPS_STALE_MINUTES"},
		{name: "lock off from the user file", user: `{"lock":{"mode":"none"}}`, wantIn: "lock.mode none is accepted only from .bops.json"},
	}
	for _, c := range cases {
		for _, cmd := range []string{"next", "sweep", "doctor"} {
			t.Run(c.name+"/"+cmd, func(t *testing.T) {
				configDir(t, c.repo)
				if c.env != "" {
					t.Setenv("BOPS_STALE_MINUTES", c.env)
				}
				if c.user != "" {
					p := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "bops", "config.json")
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte(c.user), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				stdout, _ := os.CreateTemp(t.TempDir(), "out")
				stderr, _ := os.CreateTemp(t.TempDir(), "err")
				code := run([]string{cmd, "--json"}, stdout, stderr)
				b, _ := os.ReadFile(stdout.Name())
				got := map[string]any{}
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatalf("stdout is not JSON: %v: %s", err, b)
				}
				if code != exitCantRun || got["ok"] != false || got["code"] != lock.CodeBadConfig {
					t.Fatalf("exit %d, ok %v, code %v; want exit 1, ok false, code %s", code, got["ok"], got["code"], lock.CodeBadConfig)
				}
				if !strings.Contains(fmt.Sprint(got["error"]), c.wantIn) {
					t.Errorf("error = %v, want it to contain %q", got["error"], c.wantIn)
				}
			})
		}
	}
}

func TestNewEnvRefusesABadConfig(t *testing.T) {
	configDir(t, `{"stale_minutes":"soon"}`)
	_, _, err := newEnv(runstate.Read)
	if err == nil || !strings.Contains(err.Error(), ".bops.json") || !strings.Contains(err.Error(), "stale_minutes") {
		t.Fatalf("err = %v, want it to name .bops.json and stale_minutes", err)
	}
}
