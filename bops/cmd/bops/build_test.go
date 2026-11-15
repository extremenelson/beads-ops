package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVcsOf(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "abc123"},
		{Key: "vcs.time", Value: "2026-11-14T10:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	if got := vcsOf(info); got != (buildVCS{Revision: "abc123", Modified: true, Time: "2026-11-14T10:00:00Z"}) {
		t.Errorf("vcsOf = %+v", got)
	}
	if got := vcsOf(nil); got != (buildVCS{}) {
		t.Errorf("vcsOf(nil) = %+v", got)
	}
}

// bopsCheckout makes a git repo shaped like beads-ops: bops/go.mod naming
// bopsModule. It commits a (bops/), b (bops/), then c (docs only), and
// returns their commits.
func bopsCheckout(t *testing.T, dir, module string) (a, b, c string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	commit := func(path, body string) string {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", path)
		git(t, dir, "-c", "user.name=t", "-c", "user.email=t@invalid", "commit", "-q", "-m", path)
		rev, err := gitOut(context.Background(), dir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return rev
	}
	if _, err := gitOut(context.Background(), dir, "rev-parse", "--git-dir"); err != nil {
		git(t, dir, "init", "-q")
	}
	a = commit("bops/go.mod", "module "+module+"\n\ngo 1.25\n")
	b = commit("bops/x.go", "package x\n")
	c = commit("README.md", "docs\n")
	return a, b, c
}

func TestStaleBinary(t *testing.T) {
	dir := t.TempDir()
	a, b, c := bopsCheckout(t, dir, bopsModule)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		rev  string
		want []string // substrings; none means no warning
	}{
		{"built at the last bops/ change", b, nil},
		{"built after it (a docs-only commit)", c, nil},
		{"built before it", a, []string{"built from " + short(a), "before " + short(b), "go install ./cmd/bops"}},
		{"no recorded commit", "", []string{"records no commit", "go install ./cmd/bops"}},
		{"a commit this checkout lacks", strings.Repeat("e", 40), []string{"does not have", "go install ./cmd/bops"}},
	} {
		got := staleBinary(ctx, dir, buildVCS{Revision: tc.rev})
		if len(tc.want) == 0 && got != "" {
			t.Errorf("%s: warning %q, want none", tc.name, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: warning %q, want it to contain %q", tc.name, got, w)
			}
		}
	}
}

// Other repositories run bops too; their bops/ (if any) is not bops's source.
func TestStaleBinaryIgnoresOtherRepositories(t *testing.T) {
	ctx := context.Background()
	other := t.TempDir()
	bopsCheckout(t, other, "example.com/someone/else")
	if got := staleBinary(ctx, other, buildVCS{}); got != "" {
		t.Errorf("another module's bops/: %q", got)
	}
	plain := t.TempDir()
	git(t, plain, "init", "-q")
	if got := staleBinary(ctx, plain, buildVCS{}); got != "" {
		t.Errorf("a repo with no bops/: %q", got)
	}
}

// doctor warns about a stale binary and still exits on its lock checks alone.
func TestDoctorWarnsWhenTheBinaryPredatesBops(t *testing.T) {
	var old string
	saved := runningBuild
	t.Cleanup(func() { runningBuild = saved })
	runningBuild = func() buildVCS { return buildVCS{Revision: old} }
	code, got := runDoctor(t, func(clone string) {
		old, _, _ = bopsCheckout(t, clone, bopsModule)
	})
	if code != exitVerdict || got["ok"] != true {
		t.Fatalf("a stale binary must not fail doctor: exit %d: %v", code, got)
	}
	warnings, _ := got["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "built from "+short(old)) {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestDoctorHasNoBinaryWarningElsewhere(t *testing.T) {
	saved := runningBuild
	t.Cleanup(func() { runningBuild = saved })
	runningBuild = func() buildVCS { return buildVCS{} }
	_, got := runDoctor(t, nil)
	if w, _ := got["warnings"].([]any); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}
