package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

func emitted(t *testing.T, result any, err error) (int, map[string]any) {
	t.Helper()
	m, perr := manifest.Parse(bops.ManifestJSON)
	if perr != nil {
		t.Fatal(perr)
	}
	out, cerr := os.CreateTemp(t.TempDir(), "out")
	if cerr != nil {
		t.Fatal(cerr)
	}
	stderr, cerr := os.CreateTemp(t.TempDir(), "err")
	if cerr != nil {
		t.Fatal(cerr)
	}
	code := emit(m, "claim", true, result, err, out, stderr)
	b, rerr := os.ReadFile(out.Name())
	if rerr != nil {
		t.Fatal(rerr)
	}
	got := map[string]any{}
	if jerr := json.Unmarshal(b, &got); jerr != nil {
		t.Fatalf("%v: %s", jerr, b)
	}
	return code, got
}

func TestEmitExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  any
		err     error
		code    int
		ok      bool
		errCode string
	}{
		{"claimed", &intent.ClaimOut{Outcome: intent.OutcomeClaimed}, nil, exitVerdict, true, ""},
		{"held", &intent.ClaimOut{Outcome: intent.OutcomeHeld}, nil, exitVerdict, true, ""},
		{"lost", &intent.ClaimOut{Outcome: intent.OutcomeLost}, nil, exitNegative, false, ""},
		{"refused", &intent.ClaimOut{Outcome: intent.OutcomeRefused}, nil, exitNegative, false, ""},
		{"released", &intent.ReleaseOut{Outcome: intent.OutcomeReleased}, nil, exitVerdict, true, ""},
		{"release lost", &intent.ReleaseOut{Outcome: intent.OutcomeLost}, nil, exitNegative, false, ""},
		{"unreachable", &intent.ClaimOut{Outcome: intent.OutcomeUnreachable}, &lock.Error{Code: lock.CodeUnreachable, Err: errors.New("no")}, exitCantRun, false, lock.CodeUnreachable},
		{"nil result with error", (*intent.ClaimOut)(nil), errors.New("bad"), exitCantRun, false, ""},
		{"session-start after a failed sync", &intent.SessionStartOut{Mine: []string{"x-1"}}, errors.New("sync failed"), exitCantRun, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := emitted(t, tc.result, tc.err)
			if code != tc.code || got["ok"] != tc.ok {
				t.Errorf("code=%d ok=%v, want %d %v", code, got["ok"], tc.code, tc.ok)
			}
			if r, ok := tc.result.(*intent.SessionStartOut); ok && r != nil {
				if mine, _ := got["mine"].([]any); len(mine) != 1 {
					t.Errorf("a failed sync must still print the document: %v", got)
				}
			}
			if c, _ := got["code"].(string); c != tc.errCode {
				t.Errorf("code field = %q, want %q", c, tc.errCode)
			}
		})
	}
}

func TestEmitScrubsCredentials(t *testing.T) {
	_, got := emitted(t, (*intent.ClaimOut)(nil), errors.New("fatal: unable to access 'https://user:s3cret@example.invalid/r'"))
	if strings.Contains(got["error"].(string), "s3cret") {
		t.Errorf("error = %v", got["error"])
	}
}

func TestBudgets(t *testing.T) {
	for name, min := range map[string]time.Duration{"claim": time.Minute, "release": time.Minute, "note": time.Minute, "session-start": time.Minute, "sync": 2 * time.Minute, "sweep": 5 * time.Minute, "doctor": time.Minute} {
		if budget(name) < min {
			t.Errorf("%s budget = %v", name, budget(name))
		}
	}
	if budget("next") != 30*time.Second {
		t.Errorf("next budget = %v", budget("next"))
	}
}

func TestNoteTextFromStdin(t *testing.T) {
	got, err := noteText("", strings.NewReader("from stdin\n\nNEXT: x"), false)
	if err != nil || got != "from stdin\n\nNEXT: x" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNoteTextFromFileIgnoresStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := noteText(path, strings.NewReader("from stdin"), true)
	if err != nil || got != "from file" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNoteTextMissingFile(t *testing.T) {
	if _, err := noteText(filepath.Join(t.TempDir(), "nope"), strings.NewReader(""), true); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestNoteTextRefusesATerminal(t *testing.T) {
	if _, err := noteText("", strings.NewReader(""), true); err == nil {
		t.Fatal("want an error rather than waiting on a terminal")
	}
}

// failReader fails the test if anything reads it: release with no --file must
// leave stdin alone, since a caller's stdin may be a pipe that never closes.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("stdin read without --file -")
	return 0, io.EOF
}

func TestReleaseTextIsOptionalAndLeavesStdinAlone(t *testing.T) {
	got, err := releaseText("", failReader{t})
	if err != nil || got != "" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestReleaseTextFromStdinWithDash(t *testing.T) {
	got, err := releaseText("-", strings.NewReader("BLOCKER: x"))
	if err != nil || got != "BLOCKER: x" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestReleaseTextFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "why")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := releaseText(path, failReader{t})
	if err != nil || got != "from file" {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := releaseText(filepath.Join(t.TempDir(), "nope"), failReader{t}); err == nil {
		t.Fatal("missing file should fail")
	}
}

// An empty --file is a mistake, refused before the run state or the ledger is
// touched, as an empty note is.
func TestReleaseRefusesAnEmptyTextFile(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _ := os.CreateTemp(t.TempDir(), "out")
	stderr, _ := os.CreateTemp(t.TempDir(), "err")
	code := run([]string{"release", "x-1", "--reason", "blocked", "--file", path, "--json"}, stdout, stderr)
	b, _ := os.ReadFile(stdout.Name())
	if code != exitCantRun || !strings.Contains(string(b), "release text is empty") {
		t.Fatalf("exit %d: %s", code, b)
	}
}

func TestResolveVersion(t *testing.T) {
	built := func(v string) *debug.BuildInfo {
		i := &debug.BuildInfo{}
		i.Main.Version = v
		return i
	}
	for _, tc := range []struct {
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{"1.2.3", built("v9.9.9"), "1.2.3"},
		{"dev", built("v0.1.0"), "v0.1.0"},
		{"dev", built("(devel)"), "dev"},
		{"dev", built(""), "dev"},
		{"dev", nil, "dev"},
	} {
		if got := resolveVersion(tc.stamped, tc.info); got != tc.want {
			t.Errorf("resolveVersion(%q, %+v) = %q, want %q", tc.stamped, tc.info, got, tc.want)
		}
	}
}

func TestParseAnyOrder(t *testing.T) {
	for _, args := range [][]string{
		{"x-1", "--reason", "done", "--force"},
		{"--reason", "done", "--force", "x-1"},
		{"--force", "x-1", "--reason=done"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		reason := fs.String("reason", "", "")
		force := fs.Bool("force", false, "")
		if err := parseAnyOrder(fs, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if fs.NArg() != 1 || fs.Arg(0) != "x-1" || *reason != "done" || !*force {
			t.Errorf("%v: id=%v reason=%q force=%v", args, fs.Args(), *reason, *force)
		}
	}
}

func TestParseAnyOrderKeepsExtraArguments(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Bool("force", false, "")
	if err := parseAnyOrder(fs, []string{"x-1", "--force", "x-2"}); err != nil {
		t.Fatal(err)
	}
	if fs.NArg() != 2 {
		t.Errorf("extra arguments must stay visible so usage fails: %v", fs.Args())
	}
}

func TestParseAnyOrderRejectsUnknownFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := parseAnyOrder(fs, []string{"x-1", "--nope"}); err == nil {
		t.Error("want an error for an unknown flag")
	}
}

// Commands that take no bead id refuse a stray argument as usage (exit 2)
// before any bd or git call. With PATH empty and no repository, a command
// that ran anyway would fail with exit 1 instead.
func TestStrayArgumentsAreUsageErrors(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"manifest"}
	for _, c := range m.Commands {
		if _, takesID := c.Args["id"]; !takesID {
			names = append(names, c.Name)
		}
	}
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, name := range names {
		for _, args := range [][]string{{name, "bops-1"}, {name, "--json", "bops-1"}} {
			stdout, _ := os.CreateTemp(t.TempDir(), "out")
			stderr, _ := os.CreateTemp(t.TempDir(), "err")
			code := run(args, stdout, stderr)
			msg, _ := os.ReadFile(stderr.Name())
			if code != exitUsage || !strings.Contains(string(msg), "usage: bops "+name) {
				t.Errorf("%v: exit %d, stderr %q; want usage", args, code, msg)
			}
		}
	}
	if len(names) < 7 {
		t.Errorf("only %d commands checked: %v", len(names), names)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// runDoctor runs `bops doctor --json` in a clone of a fresh bare remote,
// after prepare has had a chance to break the clone. bd lists one Dolt
// remote on the clone's origin, as lock.mode ref requires.
func runDoctor(t *testing.T, prepare func(clone string)) (int, map[string]any) {
	t.Helper()
	return runDoctorWith(t, prepare, func(origin string) string {
		return `[{"name":"origin","url":"git+file://` + origin + `"}]`
	})
}

// runDoctorWith is runDoctor with bd's `dolt remote list --json` answer made
// from the clone's origin URL; an empty answer makes the list fail.
func runDoctorWith(t *testing.T, prepare func(clone string), remotes func(origin string) string) (int, map[string]any) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // doctor reads lock.mode and lock.remote from config
	t.Setenv("BOPS_LOCK_MODE", "")
	if _, set := os.LookupEnv("BOPS_LOCK_REMOTE"); !set {
		t.Setenv("BOPS_LOCK_REMOTE", "")
	}
	root := t.TempDir()
	git(t, root, "init", "-q", "--bare", "remote.git")
	git(t, root, "clone", "-q", "remote.git", "clone")
	clone := filepath.Join(root, "clone")
	origin, _ := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output()
	fake := &bdx.Fake{Responses: map[string][]byte{}}
	if list := remotes(strings.TrimSpace(string(origin))); list != "" {
		fake.Responses["dolt remote list --json"] = []byte(list)
	}
	saved := doctorBD
	doctorBD = fake
	t.Cleanup(func() { doctorBD = saved })
	if prepare != nil {
		prepare(clone)
	}
	t.Chdir(clone)
	stdout, _ := os.CreateTemp(t.TempDir(), "out")
	stderr, _ := os.CreateTemp(t.TempDir(), "err")
	code := run([]string{"doctor", "--json"}, stdout, stderr)
	b, _ := os.ReadFile(stdout.Name())
	got := map[string]any{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return code, got
}

func TestDoctorHealthyExitsZero(t *testing.T) {
	code, got := runDoctor(t, nil)
	if code != exitVerdict || got["ok"] != true || got["command"] != "doctor" {
		t.Fatalf("exit %d: %v", code, got)
	}
	checks, _ := got["checks"].([]any)
	if len(checks) != 5 || checks[4].(map[string]any)["id"] != "dolt_remote" {
		t.Fatalf("checks = %v", checks)
	}
	for _, c := range checks {
		if c.(map[string]any)["status"] != "ok" {
			t.Errorf("check %v", c)
		}
	}
	if p, _ := got["probe_ref"].(string); !strings.HasPrefix(p, "refs/bops/doctor/") {
		t.Errorf("probe_ref = %v", got["probe_ref"])
	}
}

func TestDoctorFailingCheckExitsNonZero(t *testing.T) {
	code, got := runDoctor(t, func(clone string) { git(t, clone, "remote", "remove", "origin") })
	if code != exitCantRun || got["ok"] != false || got["code"] != lock.CodeNoOrigin {
		t.Fatalf("exit %d: %v", code, got)
	}
	if failed, _ := got["failed"].([]any); len(failed) != 1 || failed[0] != "origin" {
		t.Errorf("failed = %v", got["failed"])
	}
}

// With lock.mode none there is no lock to check, even with no remote at all.
func TestDoctorSkipsWhenTheLockIsOff(t *testing.T) {
	code, got := runDoctor(t, func(clone string) {
		git(t, clone, "remote", "remove", "origin")
		if err := os.WriteFile(filepath.Join(clone, ".bops.json"), []byte(`{"lock":{"mode":"none"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if code != exitVerdict || got["ok"] != true {
		t.Fatalf("exit %d: %v", code, got)
	}
	checks, _ := got["checks"].([]any)
	if len(checks) != 5 {
		t.Fatalf("checks = %v", checks)
	}
	for _, c := range checks {
		m := c.(map[string]any)
		if m["status"] != "skip" || !strings.Contains(m["detail"].(string), "lock.mode is none") {
			t.Errorf("check %v", m)
		}
	}
	if failed, _ := got["failed"].([]any); len(failed) != 0 {
		t.Errorf("failed = %v", got["failed"])
	}
}

func TestDoctorUsesLockRemoteFromConfig(t *testing.T) {
	t.Setenv("BOPS_LOCK_REMOTE", "upstream")
	code, got := runDoctor(t, func(clone string) { git(t, clone, "remote", "rename", "origin", "upstream") })
	if code != exitVerdict || got["ok"] != true {
		t.Fatalf("exit %d: %v", code, got)
	}
	if r, _ := got["remote"].(string); !strings.HasSuffix(r, "remote.git") {
		t.Errorf("remote = %v", got["remote"])
	}
}

func TestDoctorRefusesABadLockRemote(t *testing.T) {
	t.Setenv("BOPS_LOCK_REMOTE", "-upstream")
	code, got := runDoctor(t, nil)
	if code != exitCantRun || got["ok"] != false || !strings.Contains(fmt.Sprint(got["error"]), "BOPS_LOCK_REMOTE") {
		t.Fatalf("exit %d: %v", code, got)
	}
}

// In lock.mode ref a ledger with no Dolt remote cannot sit beside the lock,
// so doctor fails a dolt_remote check that names both fixes.
func TestDoctorFailsWithNoDoltRemote(t *testing.T) {
	code, got := runDoctorWith(t, nil, func(string) string { return "[]" })
	if code != exitCantRun || got["ok"] != false || got["code"] != lock.CodeRemoteMismatch {
		t.Fatalf("exit %d: %v", code, got)
	}
	if failed, _ := got["failed"].([]any); len(failed) != 1 || failed[0] != "dolt_remote" {
		t.Errorf("failed = %v", got["failed"])
	}
	checks, _ := got["checks"].([]any)
	last := checks[len(checks)-1].(map[string]any)
	if last["id"] != "dolt_remote" || last["status"] != "fail" || last["code"] != lock.CodeRemoteMismatch {
		t.Fatalf("check %v", last)
	}
	for _, text := range []string{fmt.Sprint(last["next"]), fmt.Sprint(got["error"])} {
		for _, want := range []string{"bd dolt remote add", "lock.mode", ".bops.json"} {
			if !strings.Contains(text, want) {
				t.Errorf("%q does not say %q", text, want)
			}
		}
	}
	if !strings.Contains(fmt.Sprint(last["detail"]), "no Dolt remote") {
		t.Errorf("detail %q does not name the missing Dolt remote", last["detail"])
	}
}

// A Dolt remote on another repository fails the same check.
func TestDoctorFailsWithAMismatchedDoltRemote(t *testing.T) {
	code, got := runDoctorWith(t, nil, func(string) string {
		return `[{"name":"origin","url":"git+https://github.com/other/repo.git"}]`
	})
	if code != exitCantRun || got["code"] != lock.CodeRemoteMismatch {
		t.Fatalf("exit %d: %v", code, got)
	}
	if failed, _ := got["failed"].([]any); len(failed) != 1 || failed[0] != "dolt_remote" {
		t.Errorf("failed = %v", got["failed"])
	}
}

// A Dolt remote list that cannot be read is not compared: the check is
// skipped and doctor still passes on the lock checks.
func TestDoctorSkipsAnUnreadableDoltRemoteList(t *testing.T) {
	code, got := runDoctorWith(t, nil, func(string) string { return "" })
	if code != exitVerdict || got["ok"] != true {
		t.Fatalf("exit %d: %v", code, got)
	}
	checks, _ := got["checks"].([]any)
	last := checks[len(checks)-1].(map[string]any)
	if last["id"] != "dolt_remote" || last["status"] != "skip" {
		t.Fatalf("check %v", last)
	}
}

// TestManifestArgsMatchFlags runs each command with -h, reads the flags its
// FlagSet defines, and checks them against the command's manifest args: `id`
// is the positional bead id, a stdin|file arg is the --file flag, and every
// other arg is a flag of its own name. --json is common to all.
func TestManifestArgsMatchFlags(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var made *flag.FlagSet
	saved := flagSetMade
	flagSetMade = func(fs *flag.FlagSet) { made = fs }
	t.Cleanup(func() { flagSetMade = saved })
	checked := 0
	for _, c := range m.Commands {
		made = nil
		stdout, _ := os.CreateTemp(t.TempDir(), "out")
		stderr, _ := os.CreateTemp(t.TempDir(), "err")
		code := run([]string{c.Name, "-h"}, stdout, stderr)
		msg, _ := os.ReadFile(stderr.Name())
		if code == exitCantRun && strings.Contains(string(msg), "not yet implemented") {
			continue // gates, deps: specified, with no flags yet to drift from
		}
		if code != exitUsage {
			t.Errorf("%s -h: exit %d, stderr %q; want usage", c.Name, code, msg)
			continue
		}
		want := map[string]bool{}
		for name, a := range c.Args {
			switch {
			case name == "id":
			case strings.HasPrefix(a.Type, "stdin|file"):
				want["file"] = true
			default:
				want[name] = true
			}
		}
		got := map[string]bool{} // version makes no FlagSet: it takes only --json
		if made != nil {
			made.VisitAll(func(f *flag.Flag) {
				if f.Name != "json" {
					got[f.Name] = true
				}
			})
		}
		for name := range want {
			if !got[name] {
				t.Errorf("%s: the manifest documents --%s; the command defines no such flag", c.Name, name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("%s: the command defines --%s; the manifest does not document it", c.Name, name)
			}
		}
		checked++
	}
	if checked < 12 {
		t.Errorf("only %d commands checked", checked)
	}
}
