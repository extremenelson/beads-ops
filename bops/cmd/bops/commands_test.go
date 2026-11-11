package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops"
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
	for name, min := range map[string]time.Duration{"claim": time.Minute, "release": time.Minute, "note": time.Minute, "session-start": time.Minute, "sync": 2 * time.Minute, "sweep": 5 * time.Minute} {
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
