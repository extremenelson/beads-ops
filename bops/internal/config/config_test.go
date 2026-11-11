package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/config"
)

// world is a repo root and an XDG config home, each optionally holding a file.
type world struct {
	repo, xdg string
	env       map[string]string
}

func newWorld(t *testing.T, repoJSON, userJSON string) *world {
	t.Helper()
	w := &world{repo: t.TempDir(), xdg: t.TempDir(), env: map[string]string{}}
	w.env["XDG_CONFIG_HOME"] = w.xdg
	if repoJSON != "" {
		write(t, filepath.Join(w.repo, ".bops.json"), repoJSON)
	}
	if userJSON != "" {
		write(t, filepath.Join(w.xdg, "bops", "config.json"), userJSON)
	}
	return w
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *world) load() (config.Config, error) {
	return config.Load(w.repo, func(k string) string { return w.env[k] })
}

func TestDefaultsWithNoFiles(t *testing.T) {
	c, err := newWorld(t, "", "").load()
	if err != nil {
		t.Fatal(err)
	}
	if c.StaleAfter() != 120*time.Minute || c.HoldFor() != 120*time.Minute {
		t.Errorf("stale=%v hold=%v, want 120m each", c.StaleAfter(), c.HoldFor())
	}
	if c.Forge != "auto" || c.Pool != "" || c.Caps != nil || c.LintStrict || c.NoteRequireNext {
		t.Errorf("defaults = %+v", c)
	}
}

func TestPrecedenceEnvOverUserOverRepo(t *testing.T) {
	w := newWorld(t,
		`{"pool":"repo","caps":["a"],"stale_minutes":30,"hold_minutes":45,"forge":"github","lint":{"strict":true,"disable":["W05"]},"note":{"require_next":true}}`,
		`{"pool":"user","stale_minutes":60}`)
	w.env["BOPS_STALE_MINUTES"] = "90"
	w.env["BOPS_CAPS"] = "gpu, net"
	c, err := w.load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool != "user" {
		t.Errorf("pool = %q, want user over repo", c.Pool)
	}
	if c.StaleAfter() != 90*time.Minute {
		t.Errorf("stale = %v, want env 90m over user and repo", c.StaleAfter())
	}
	if c.HoldFor() != 45*time.Minute || c.Forge != "github" || !c.LintStrict || !c.NoteRequireNext {
		t.Errorf("repo keys not kept: %+v", c)
	}
	if !slices.Equal(c.Caps, []string{"gpu", "net"}) || !slices.Equal(c.LintDisable, []string{"W05"}) {
		t.Errorf("caps=%v disable=%v", c.Caps, c.LintDisable)
	}
}

func TestLockRemote(t *testing.T) {
	c, err := newWorld(t, "", "").load()
	if err != nil || c.LockRemote != "origin" {
		t.Fatalf("default lock.remote = %q, %v; want origin", c.LockRemote, err)
	}
	w := newWorld(t, `{"lock":{"remote":"upstream"}}`, "")
	if c, err = w.load(); err != nil || c.LockRemote != "upstream" {
		t.Fatalf("repo lock.remote = %q, %v; want upstream", c.LockRemote, err)
	}
	w.env["BOPS_LOCK_REMOTE"] = "team_mirror-2"
	if c, err = w.load(); err != nil || c.LockRemote != "team_mirror-2" {
		t.Fatalf("env lock.remote = %q, %v; want team_mirror-2 over the file", c.LockRemote, err)
	}
}

func TestUserConfigFallsBackToHomeDotConfig(t *testing.T) {
	w := newWorld(t, "", "")
	home := t.TempDir()
	write(t, filepath.Join(home, ".config", "bops", "config.json"), `{"hold_minutes":5}`)
	delete(w.env, "XDG_CONFIG_HOME")
	w.env["HOME"] = home
	c, err := w.load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HoldFor() != 5*time.Minute {
		t.Errorf("hold = %v, want 5m from ~/.config", c.HoldFor())
	}
}

func TestRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name, repo string
		env        map[string]string
		want       string
	}{
		{"unknown key", `{"stale_minuets":10}`, nil, `unknown key "stale_minuets"`},
		{"unknown nested key", `{"lint":{"strikt":true}}`, nil, `unknown key "lint.strikt"`},
		{"wrong type", `{"stale_minutes":"10"}`, nil, `stale_minutes: want a whole number of minutes`},
		{"fraction", `{"stale_minutes":1.5}`, nil, `stale_minutes: want a whole number of minutes`},
		{"zero", `{"hold_minutes":0}`, nil, `hold_minutes: want at least 1`},
		{"caps not a list", `{"caps":"gpu"}`, nil, `caps: want a list of strings`},
		{"bad forge", `{"forge":"gitea"}`, nil, `forge: want one of auto, github, gitlab, none`},
		{"section not an object", `{"lint":true}`, nil, `lint: want an object`},
		{"invalid json", `{"pool":`, nil, `.bops.json`},
		{"bad env int", "", map[string]string{"BOPS_STALE_MINUTES": "2h"}, `BOPS_STALE_MINUTES: want a whole number of minutes`},
		{"bad env bool", "", map[string]string{"BOPS_LINT_STRICT": "yes please"}, `BOPS_LINT_STRICT: want true or false`},
		{"empty lock remote", `{"lock":{"remote":""}}`, nil, `lock.remote: want a git remote name`},
		{"lock remote not a string", `{"lock":{"remote":7}}`, nil, `lock.remote: want a string`},
		{"lock remote looks like a flag", `{"lock":{"remote":"-upstream"}}`, nil, `lock.remote: want a git remote name`},
		{"lock remote is a URL", "", map[string]string{"BOPS_LOCK_REMOTE": "git@github.com:a/b.git"}, `BOPS_LOCK_REMOTE: lock.remote: want a git remote name`},
		{"lock remote with a space", "", map[string]string{"BOPS_LOCK_REMOTE": "my remote"}, `BOPS_LOCK_REMOTE`},
		{"lock remote with dots", "", map[string]string{"BOPS_LOCK_REMOTE": "a..b"}, `BOPS_LOCK_REMOTE`},
		{"lock remote ending .lock", "", map[string]string{"BOPS_LOCK_REMOTE": "up.lock"}, `BOPS_LOCK_REMOTE`},
	} {
		w := newWorld(t, tc.repo, "")
		for k, v := range tc.env {
			w.env[k] = v
		}
		_, err := w.load()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestBadUserFileNamesItsPath(t *testing.T) {
	w := newWorld(t, "", `{"nope":1}`)
	_, err := w.load()
	if err == nil || !strings.Contains(err.Error(), filepath.Join("bops", "config.json")) {
		t.Fatalf("err = %v, want the user file's path", err)
	}
}

func TestLockModeDefaultsToRef(t *testing.T) {
	c, err := newWorld(t, "", "").load()
	if err != nil || c.LockMode != config.LockRef || c.LockOff() {
		t.Fatalf("lock mode %q, off=%v, err %v", c.LockMode, c.LockOff(), err)
	}
}

func TestLockModeNoneFromTheRepoFile(t *testing.T) {
	w := newWorld(t, `{"lock":{"mode":"none"}}`, "")
	w.env["BOPS_LOCK_MODE"] = "none" // repeating the repo's choice is allowed
	c, err := w.load()
	if err != nil || c.LockMode != config.LockNone || !c.LockOff() {
		t.Fatalf("lock mode %q, err %v", c.LockMode, err)
	}
}

// Turning the lock off on one machine of a shared ledger would silently drop
// that machine's protection, so only the committed file may turn it off.
func TestLockModeNoneRefusedOutsideTheRepoFile(t *testing.T) {
	w := newWorld(t, "", `{"lock":{"mode":"none"}}`)
	if _, err := w.load(); err == nil || !strings.Contains(err.Error(), "config.json") || !strings.Contains(err.Error(), ".bops.json") {
		t.Errorf("user file: err = %v, want it to name the file and point at .bops.json", err)
	}
	w = newWorld(t, "", "")
	w.env["BOPS_LOCK_MODE"] = "none"
	if _, err := w.load(); err == nil || !strings.Contains(err.Error(), "BOPS_LOCK_MODE") || !strings.Contains(err.Error(), ".bops.json") {
		t.Errorf("env: err = %v, want it to name BOPS_LOCK_MODE and point at .bops.json", err)
	}
}

func TestLockModeRefMayOverrideTheRepoFile(t *testing.T) {
	w := newWorld(t, `{"lock":{"mode":"none"}}`, "")
	w.env["BOPS_LOCK_MODE"] = "ref"
	c, err := w.load()
	if err != nil || c.LockMode != config.LockRef {
		t.Fatalf("lock mode %q, err %v", c.LockMode, err)
	}
}

func TestLockModeRejectsUnknownValue(t *testing.T) {
	_, err := newWorld(t, `{"lock":{"mode":"off"}}`, "").load()
	if err == nil || !strings.Contains(err.Error(), "lock.mode") {
		t.Fatalf("err = %v", err)
	}
}
