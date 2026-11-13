package bdx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeBD writes a stand-in for bd that asks git, as bd's own git children
// would, whether automatic maintenance may detach.
func fakeBD(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bd")
	script := "#!/bin/sh\ngit config --get maintenance.autoDetach\ngit config --get gc.autoDetach\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A detached `git repack -d` in Dolt's git remote cache can delete a pack
// while the next bd command reads it (bops-3tm). Every bd call must keep
// git's automatic maintenance in the foreground.
func TestExecKeepsGitMaintenanceInTheForeground(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "")
	out, err := Exec{Bin: fakeBD(t)}.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, []string{"false", "false"}) {
		t.Fatalf("maintenance.autoDetach, gc.autoDetach seen by bd's git = %q, want false, false", got)
	}
}

// The caller's own GIT_CONFIG_* entries survive; ours are appended after them.
func TestExecKeepsTheCallersGitConfigEntries(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "caller")
	p := filepath.Join(t.TempDir(), "bd")
	script := "#!/bin/sh\ngit config --get user.name\ngit config --get maintenance.autoDetach\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Exec{Bin: p}.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, []string{"caller", "false"}) {
		t.Fatalf("got %q, want the caller's user.name and maintenance.autoDetach=false", got)
	}
}

func TestGitEnv(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string // entries that must be present
		not  []string // entries that must be absent
	}{
		{
			name: "no existing entries",
			in:   []string{"PATH=/bin"},
			want: []string{"PATH=/bin", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.autoDetach", "GIT_CONFIG_VALUE_0=false", "GIT_CONFIG_KEY_1=gc.autoDetach", "GIT_CONFIG_VALUE_1=false"},
		},
		{
			name: "appends after the caller's entries",
			in:   []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=x"},
			want: []string{"GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=x", "GIT_CONFIG_KEY_1=maintenance.autoDetach", "GIT_CONFIG_KEY_2=gc.autoDetach"},
			not:  []string{"GIT_CONFIG_COUNT=1"},
		},
		{
			name: "an empty count is no entries",
			in:   []string{"GIT_CONFIG_COUNT="},
			want: []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.autoDetach"},
			not:  []string{"GIT_CONFIG_COUNT="},
		},
		{
			name: "an unreadable count is left for git to report",
			in:   []string{"GIT_CONFIG_COUNT=lots"},
			want: []string{"GIT_CONFIG_COUNT=lots"},
			not:  []string{"GIT_CONFIG_KEY_0=maintenance.autoDetach"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := GitEnv(c.in)
			for _, w := range c.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, n := range c.not {
				if slices.Contains(got, n) {
					t.Errorf("unexpected %q in %q", n, got)
				}
			}
		})
	}
}
