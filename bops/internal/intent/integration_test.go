package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// These tests run claim and release against a real bd and a throwaway
// ledger, because the fake cannot catch a flag combination bd refuses.
// They need bd on PATH and run only with BOPS_INTEGRATION=1.

const itActor = "it-actor"

func liveLedger(t *testing.T) (*intent.Env, func(title string) string) {
	t.Helper()
	if os.Getenv("BOPS_INTEGRATION") != "1" {
		t.Skip("set BOPS_INTEGRATION=1 to run against a real bd")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Fatal("BOPS_INTEGRATION=1 but bd is not on PATH")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("BEADS_DIR", "")
	t.Setenv("BEADS_DB", "")
	t.Setenv("BEADS_ACTOR", itActor)

	sh := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	sh("git", "init", "-q")
	sh("git", "config", "user.email", "it@example.invalid")
	sh("git", "config", "user.name", itActor)
	sh("git", "init", "-q", "--bare", filepath.Join(t.TempDir(), "origin.git"))
	bare, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), "*", "origin.git"))
	sh("git", "remote", "add", "origin", bare[len(bare)-1])
	sh("git", "commit", "-q", "--allow-empty", "-m", "init")
	sh("git", "push", "-q", "origin", "HEAD:refs/heads/main")
	sh("bd", "init", "--quiet", "--prefix", "it")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	w, err := runstate.Open(dir, runstate.Write)
	if err != nil {
		t.Fatal(err)
	}
	w.Run().ID, w.Run().Actor, w.Run().Machine = "ITRUN", itActor, "it-box"
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	store, err := runstate.Open(dir, runstate.Read)
	if err != nil {
		t.Fatal(err)
	}

	bd := bdx.Exec{}
	env := &intent.Env{
		BD:    bd,
		Run:   store.Run(),
		Store: store,
		Sleep: func(time.Duration) {},
		Lock:  &lock.Lock{Git: lock.Exec{Dir: dir}, Journal: store, Run: "ITRUN", Actor: itActor},
	}
	create := func(title string) string {
		t.Helper()
		raw, err := bd.Run(context.Background(), "create", title, "--type", "task", "--acceptance", "it works", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &got); err != nil || got.ID == "" {
			t.Fatalf("decode bd create output %q: %v", raw, err)
		}
		return got.ID
	}
	return env, create
}

func liveShow(t *testing.T, env *intent.Env, id string) *ledger.Issue {
	t.Helper()
	raw, err := env.BD.Run(context.Background(), "show", id, "--json")
	if err != nil {
		t.Fatal(err)
	}
	i, err := ledger.DecodeIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func liveReady(t *testing.T, env *intent.Env) []string {
	t.Helper()
	raw, err := env.BD.Run(context.Background(), "ready", "--json")
	if err != nil {
		t.Fatal(err)
	}
	issues, err := ledger.DecodeIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(issues))
	for _, i := range issues {
		ids = append(ids, i.ID)
	}
	return ids
}

func TestLiveClaimAndRelease(t *testing.T) {
	env, create := liveLedger(t)
	ctx := context.Background()

	for _, tc := range []struct {
		reason, status, label string
		ready                 bool
	}{
		{"scope", "open", "bops:hold", true},
		{"stale", "open", "bops:hold", true},
		{"human", "open", "needs:human", true},
		{"blocked", "blocked", "", false},
		{"done", "in_progress", "bops:landing", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			id := create("release " + tc.reason)

			out, err := intent.Claim(ctx, env, id, false)
			if err != nil {
				t.Fatalf("claim without --force: %v", err)
			}
			if !out.ReadbackOK {
				t.Fatalf("readback failed: %+v", out)
			}
			got := liveShow(t, env, id)
			if got.Status != "in_progress" || got.Assignee == "" || !got.HasLabel("run:ITRUN") {
				t.Fatalf("after claim: status=%s assignee=%q labels=%v", got.Status, got.Assignee, got.Labels)
			}

			if _, err := intent.Release(ctx, env, id, tc.reason); err != nil {
				t.Fatalf("release: %v", err)
			}
			got = liveShow(t, env, id)
			if got.Status != tc.status {
				t.Errorf("status = %s, want %s", got.Status, tc.status)
			}
			if got.Assignee != "" {
				t.Errorf("assignee = %q, want empty", got.Assignee)
			}
			if got.HasLabel("run:ITRUN") {
				t.Errorf("run label should be gone: %v", got.Labels)
			}
			if tc.label != "" && !got.HasLabel(tc.label) {
				t.Errorf("missing label %s: %v", tc.label, got.Labels)
			}
			if in := slices.Contains(liveReady(t, env), id); in != tc.ready {
				t.Errorf("in bd ready = %v, want %v", in, tc.ready)
			}
		})
	}
}

func TestLiveNote(t *testing.T) {
	env, create := liveLedger(t)
	ctx := context.Background()
	id := create("note target")
	if _, err := intent.Claim(ctx, env, id, false); err != nil {
		t.Fatal(err)
	}

	text := "line with \"quotes\" and $(no expansion)\n\nNEXT: keep going"
	out, err := intent.Note(ctx, env, id, text)
	if err != nil {
		t.Fatal(err)
	}
	if out.CommentID == "" || len(out.Warnings) != 0 {
		t.Fatalf("%+v", out)
	}
	raw, err := env.BD.Run(ctx, "comments", id, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var comments []struct {
		ID, Text string
	}
	if err := json.Unmarshal(raw, &comments); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range comments {
		found = found || (c.ID == out.CommentID && c.Text == "bops:note run=ITRUN\n"+text)
	}
	if !found {
		t.Errorf("note not stored verbatim: %+v", comments)
	}

	other := *env
	other.Run = &runstate.Run{ID: "OTHER", Actor: itActor, Machine: "it-box"}
	if _, err := intent.Note(ctx, &other, id, text); !errors.Is(err, intent.ErrPrecondition) {
		t.Errorf("another run should be refused, got %v", err)
	}
}

func TestLiveHoldWindowAndReclaim(t *testing.T) {
	env, create := liveLedger(t)
	ctx := context.Background()
	id := create("held after scope release")

	if _, err := intent.Claim(ctx, env, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Release(ctx, env, id, "scope"); err != nil {
		t.Fatal(err)
	}

	candidates := func(now time.Time) []string {
		t.Helper()
		env.Now = func() time.Time { return now }
		out, err := intent.Next(ctx, env, intent.NextOpts{N: 10})
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, c := range out.Candidates {
			ids = append(ids, c.ID)
		}
		return ids
	}
	now := time.Now().UTC()
	if slices.Contains(candidates(now), id) {
		t.Errorf("next offered %s during its hold", id)
	}
	if !slices.Contains(candidates(now.Add(3*time.Hour)), id) {
		t.Errorf("next did not offer %s after its hold ended", id)
	}

	env.Now = nil
	if _, err := intent.Claim(ctx, env, id, false); err != nil {
		t.Fatalf("re-claim after hold: %v", err)
	}
	if got := liveShow(t, env, id); got.HasLabel("bops:hold") {
		t.Errorf("claim should clear bops:hold: %v", got.Labels)
	}
}
