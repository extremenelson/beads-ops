package intent

import (
	"strings"
	"testing"
	"time"
)

// Only a name carrying the whole id counts, a remote first; this checkout's
// own branch counts only on a remote; worktree directory names are not
// branches.
func TestPickBranchRanks(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []workBranch
		here string
		want string
	}{
		{"remote over local", []workBranch{{"work/x-1", "local"}, {"feat/x-1", "remote origin"}}, "", "feat/x-1"},
		{"suffix only is no match", []workBranch{{"fix-1", "remote origin"}, {"release/1.2", "remote origin"}}, "", ""},
		{"whole id, local only", []workBranch{{"work/x-1", "local"}}, "", "work/x-1"},
		{"this checkout's branch skipped locally", []workBranch{{"work/x-1", "local"}, {"work/x-1", "worktree /w/x-1"}}, "work/x-1", ""},
		{"this checkout's branch on a remote", []workBranch{{"work/x-1", "local"}, {"work/x-1", "remote origin"}}, "work/x-1", "work/x-1"},
		{"directory skipped", []workBranch{{"x-1", "worktree directory /w/x-1"}}, "", ""},
		{"other bead", []workBranch{{"work/x-10", "remote origin"}}, "", ""},
		{"child bead", []workBranch{{"work/x-1.2", "remote origin"}, {"x-1.3-foo", "local"}}, "", ""},
		{"dotted suffix is the bead", []workBranch{{"work/x-1.wip", "local"}}, "", "work/x-1.wip"},
		{"live remote over last fetch", []workBranch{{"old/x-1", "remote fork (last fetch)"}, {"work/x-1", "remote origin"}}, "", "work/x-1"},
		{"last fetch over local", []workBranch{{"work/x-1", "local"}, {"old/x-1", "remote fork (last fetch)"}}, "", "old/x-1"},
	} {
		if got := pickBranch(c.in, "x-1", c.here); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := pickBranch([]workBranch{{"work/x-1", "remote origin"}}, "x-1.2", ""); got != "" {
		t.Errorf("parent branch: got %q", got)
	}
}

// created_at decides which note is newest when both have one, whatever
// order bd lists them in; the previous holder's own note wins over a newer
// one by another run.
func TestLastNote(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	comments := []bdComment{
		{Text: notePrefix + "B\nNEXT: by B, later", CreatedAt: t0.Add(time.Hour)},
		{Text: notePrefix + "B\nNEXT: by B, earlier", CreatedAt: t0},
		{Text: notePrefix + "A\nNEXT: by A", CreatedAt: t0.Add(30 * time.Minute)},
		{Text: "not a note", CreatedAt: t0.Add(2 * time.Hour)},
	}
	if got, by := lastNote(comments, ""); got != "NEXT: by B, later" || by != "B" {
		t.Errorf("newest: %q by %q", got, by)
	}
	if got, by := lastNote(comments, "A"); got != "NEXT: by A" || by != "A" {
		t.Errorf("by holder: %q by %q", got, by)
	}
	if got, by := lastNote(comments, "C"); got != "NEXT: by B, later" || by != "B" {
		t.Errorf("holder wrote none: %q by %q", got, by)
	}
	mixed := []bdComment{
		{Text: notePrefix + "A\nNEXT: first", CreatedAt: t0.Add(time.Hour)},
		{Text: notePrefix + "A\nNEXT: no time"},
		{Text: notePrefix + "A\nNEXT: last"},
	}
	if got, _ := lastNote(mixed, "A"); got != "NEXT: last" {
		t.Errorf("mixed times go by position: %q", got)
	}
	long, _ := lastNote([]bdComment{{Text: notePrefix + "A\nNEXT: " + strings.Repeat("é", 3000)}}, "A")
	if r := []rune(long); len(r) != maxLastNote+len(" [truncated]") || !strings.HasSuffix(long, " [truncated]") {
		t.Errorf("long note: %d runes", len(r))
	}
	if got := handoffParagraph("a\r\n\r\nBLOCKER: x\r\n\r\nNEXT: y\n\n  \n"); got != "NEXT: y" {
		t.Errorf("paragraph: %q", got)
	}
}
