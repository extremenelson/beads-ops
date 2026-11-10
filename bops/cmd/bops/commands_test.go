package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

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
