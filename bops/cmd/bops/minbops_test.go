package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// runningAs makes this binary report v for the rest of the test.
func runningAs(t *testing.T, v string) {
	t.Helper()
	old := runningVersion
	runningVersion = func() string { return v }
	t.Cleanup(func() { runningVersion = old })
}

// runDoc runs a command with --json and decodes its document.
func runDoc(t *testing.T, args ...string) (int, map[string]any, string) {
	t.Helper()
	stdout, _ := os.CreateTemp(t.TempDir(), "out")
	stderr, _ := os.CreateTemp(t.TempDir(), "err")
	code := run(append(args, "--json"), stdout, stderr)
	b, _ := os.ReadFile(stdout.Name())
	e, _ := os.ReadFile(stderr.Name())
	got := map[string]any{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: stdout is not JSON: %v: %s", args, err, b)
	}
	return code, got, string(e)
}

// A write by a bops older than min_bops is refused before it touches the
// ledger or the lock, with a stable code and the upgrade command (bops-n4r).
func TestMinBopsRefusesWrites(t *testing.T) {
	for _, args := range [][]string{{"claim", "x-1"}, {"release", "x-1", "--reason", "done"}, {"land", "x-1", "--ref", "r", "--sha", "s"}, {"sync"}, {"sweep"}, {"session-start"}} {
		t.Run(args[0], func(t *testing.T) {
			configDir(t, `{"min_bops":"0.6.0"}`)
			runningAs(t, "v0.5.1")
			code, got, _ := runDoc(t, args...)
			if code != exitCantRun || got["ok"] != false || got["code"] != lock.CodeTooOld {
				t.Fatalf("exit %d, ok %v, code %v; want exit 1, ok false, code %s", code, got["ok"], got["code"], lock.CodeTooOld)
			}
			if e := fmt.Sprint(got["error"]); !strings.Contains(e, "this bops is v0.5.1, older than min_bops 0.6.0") || !strings.Contains(e, "go install "+bopsModule+"/cmd/bops@v0.6.0") {
				t.Errorf("error = %s, want the versions and the install command", e)
			}
		})
	}
}

func TestMinBopsRefusesSessionStartAsHookOutput(t *testing.T) {
	configDir(t, `{"min_bops":"0.6.0"}`)
	runningAs(t, "v0.5.1")
	stdout, _ := os.CreateTemp(t.TempDir(), "out")
	stderr, _ := os.CreateTemp(t.TempDir(), "err")
	code := run([]string{"session-start", "--hook-json"}, stdout, stderr)
	b, _ := os.ReadFile(stdout.Name())
	if code != exitVerdict || !strings.Contains(string(b), "hookSpecificOutput") || !strings.Contains(string(b), lock.CodeTooOld) {
		t.Fatalf("exit %d, stdout %s; want exit 0 hook output naming %s", code, b, lock.CodeTooOld)
	}
}

// A read still answers, with the floor as a warning in its document.
func TestMinBopsWarnsOnReads(t *testing.T) {
	configDir(t, `{"min_bops":"0.6.0"}`)
	runningAs(t, "v0.5.1")
	_, got, _ := runDoc(t, "next")
	if got["code"] == lock.CodeTooOld {
		t.Fatalf("a read was refused: %v", got)
	}
	if ws := fmt.Sprint(got["warnings"]); !strings.Contains(ws, "older than min_bops 0.6.0") {
		t.Errorf("warnings = %s, want the min_bops warning", ws)
	}
}

// At or above the floor, and with no version to compare, nothing changes.
func TestMinBopsLetsMetAndUnversionedBuildsRun(t *testing.T) {
	for _, v := range []string{"v0.6.0", "v0.7.1", "dev"} {
		t.Run(v, func(t *testing.T) {
			configDir(t, `{"min_bops":"0.6.0"}`)
			runningAs(t, v)
			_, got, _ := runDoc(t, "claim", "x-1")
			if got["code"] == lock.CodeTooOld {
				t.Fatalf("bops %s was refused: %v", v, got)
			}
			_, got, _ = runDoc(t, "next")
			if ws := fmt.Sprint(got["warnings"]); strings.Contains(ws, "min_bops") {
				t.Errorf("bops %s: warnings = %s, want none about min_bops", v, ws)
			}
		})
	}
}

func TestMinBopsCheck(t *testing.T) {
	if c, w := minBopsCheck("", "v0.1.0"); c != nil || w != "" {
		t.Errorf("no min_bops: %v, %q; want nothing", c, w)
	}
	if c, _ := minBopsCheck("0.6.0", "v0.5.1"); c == nil || c.Status != lock.CheckFail || c.Code != lock.CodeTooOld || !strings.Contains(c.Next, "@v0.6.0") {
		t.Errorf("older: %+v, want a failed bops_version check with the install command", c)
	}
	if c, _ := minBopsCheck("0.6.0", "v0.6.0"); c == nil || c.Status != lock.CheckOK {
		t.Errorf("met: %+v, want an ok check", c)
	}
	if c, w := minBopsCheck("0.6.0", "dev"); c != nil || !strings.Contains(w, "cannot be checked") {
		t.Errorf("dev: %v, %q; want only a warning", c, w)
	}
}

// doctor lists the failed bops_version check among its checks.
func TestDoctorFailsBelowMinBops(t *testing.T) {
	configDir(t, `{"min_bops":"0.6.0","lock":{"mode":"none"}}`)
	runningAs(t, "v0.5.1")
	code, got, _ := runDoc(t, "doctor")
	if code != exitCantRun || got["code"] != lock.CodeTooOld {
		t.Fatalf("exit %d, code %v; want exit 1 and %s", code, got["code"], lock.CodeTooOld)
	}
	if f := fmt.Sprint(got["failed"]); !strings.Contains(f, "bops_version") {
		t.Errorf("failed = %s, want bops_version", f)
	}
}
