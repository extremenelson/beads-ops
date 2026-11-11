package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// These tests use real git against a bare repository and a real state file.
// Only bd is faked, by a small stateful ledger. The live suite covers bd itself.

var bg = context.Background()

type bead struct {
	Status, Assignee string
	Labels, Comments []string
	CommentTimes     []time.Time // created_at of Comments[i], when set
	Metadata         json.RawMessage
	Updated          time.Time
	Heartbeat        *time.Time
}

type fakeBD struct {
	mu      sync.Mutex
	actor   string
	beads   map[string]*bead
	remotes string
	calls   [][]string
	fail    func(args []string) error
	onPull  func()
	ready   []byte // bd ready output; nil means two fixed beads
	prime   string
}

func newBD() *fakeBD {
	return &fakeBD{actor: "tester", beads: map[string]*bead{}, remotes: "[]"}
}

func (b *fakeBD) add(id string) *bead {
	n := &bead{Status: "open", Updated: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	b.beads[id] = n
	return n
}

func (b *fakeBD) wrote(id string) bool {
	for _, c := range b.calls {
		if (c[0] == "update" || c[0] == "label" || (c[0] == "comments" && c[1] == "add")) && slices.Contains(c, id) {
			return true
		}
	}
	return false
}

func (b *fakeBD) count(prefix string) int {
	n := 0
	for _, c := range b.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			n++
		}
	}
	return n
}

func (b *fakeBD) Run(_ context.Context, args ...string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, args)
	if b.fail != nil {
		if err := b.fail(args); err != nil {
			return nil, err
		}
	}
	switch args[0] {
	case "dolt":
		switch args[1] {
		case "remote":
			return []byte(b.remotes), nil
		case "pull":
			if b.onPull != nil {
				b.onPull()
			}
		}
		return nil, nil
	case "heartbeat":
		return nil, nil
	case "show":
		n, ok := b.beads[args[1]]
		if !ok {
			return nil, fmt.Errorf("no bead %s", args[1])
		}
		return json.Marshal([]map[string]any{b.issue(args[1], n)})
	case "comments":
		if args[1] == "add" {
			if args[3] == "-f" {
				text, err := os.ReadFile(args[4])
				if err != nil {
					return nil, err
				}
				b.beads[args[2]].Comments = append(b.beads[args[2]].Comments, string(text))
				return []byte(`{"id":"c-1"}`), nil
			}
			b.beads[args[2]].Comments = append(b.beads[args[2]].Comments, args[3])
			return nil, nil
		}
		out := []map[string]string{}
		n := b.beads[args[1]]
		for k, c := range n.Comments {
			m := map[string]string{"text": c}
			if k < len(n.CommentTimes) {
				m["created_at"] = n.CommentTimes[k].Format(time.RFC3339)
			}
			out = append(out, m)
		}
		return json.Marshal(out)
	case "label":
		n := b.beads[args[2]]
		if args[1] == "add" {
			if !slices.Contains(n.Labels, args[3]) {
				n.Labels = append(n.Labels, args[3])
			}
		} else {
			n.Labels = slices.DeleteFunc(n.Labels, func(l string) bool { return l == args[3] })
		}
		return nil, nil
	case "update":
		n := b.beads[args[1]]
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--metadata":
				raw, err := os.ReadFile(strings.TrimPrefix(args[i+1], "@"))
				if err != nil {
					return nil, err
				}
				n.Metadata = raw
				i++
			case "--claim":
				n.Assignee, n.Status = b.actor, "in_progress"
			case "--assignee":
				n.Assignee = args[i+1]
				i++
			case "--status":
				n.Status = args[i+1]
				i++
			}
		}
		return nil, nil
	case "prime":
		return []byte(b.prime), nil
	case "gate":
		return []byte(`[]`), nil
	case "ready":
		if b.ready != nil {
			return b.ready, nil
		}
		return []byte(`[{"id":"x-1","title":"t","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"},
			{"id":"x-2","title":"t","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-02T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`), nil
	case "list":
		if slices.Contains(args, "--limit") {
			out := []map[string]any{}
			for id, n := range b.beads {
				if n.Status == "in_progress" {
					out = append(out, b.issue(id, n))
				}
			}
			return json.Marshal(out)
		}
		return []byte(`[]`), nil
	case "close":
		b.beads[args[1]].Status = "closed"
		return nil, nil
	}
	return nil, fmt.Errorf("fakeBD: unhandled %v", args)
}

func (b *fakeBD) issue(id string, n *bead) map[string]any {
	out := map[string]any{"id": id, "status": n.Status, "assignee": n.Assignee, "labels": n.Labels,
		"created_at": "2026-10-01T00:00:00Z", "updated_at": n.Updated.Format(time.RFC3339)}
	if n.Metadata != nil {
		out["metadata"] = n.Metadata
	}
	if n.Heartbeat != nil {
		out["heartbeat_at"] = n.Heartbeat.Format(time.RFC3339)
	}
	return out
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func bareRemote(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BEADS_ACTOR", "tester")
	path := filepath.Join(t.TempDir(), "remote.git")
	git(t, filepath.Dir(path), "init", "-q", "--bare", path)
	return path
}

// openStore opens dir's state with a known run id.
func openStore(t *testing.T, dir, run string) *runstate.Store {
	t.Helper()
	w, err := runstate.Open(dir, runstate.Write)
	if err != nil {
		t.Fatal(err)
	}
	w.Run().ID, w.Run().Actor, w.Run().Machine = run, "tester", "box"
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	s, err := runstate.Open(dir, runstate.Read)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// flakyGit fails the pushes and remote reads of a side that has gone offline.
type flakyGit struct {
	lock.Exec
	offline    bool
	beforePush func()
}

func (g *flakyGit) Run(ctx context.Context, c lock.Cmd) (lock.Output, error) {
	if c.Args[0] == "push" && g.beforePush != nil {
		hook := g.beforePush
		g.beforePush = nil
		hook()
	}
	if g.offline && (c.Args[0] == "push" || c.Args[0] == "ls-remote" || c.Args[0] == "fetch") {
		return lock.Output{Stderr: "fatal: unable to access remote"}, errors.New("exit status 128")
	}
	return g.Exec.Run(ctx, c)
}

type rig struct {
	env   *intent.Env
	bd    *fakeBD
	store *runstate.Store
	git   *flakyGit
	dir   string
}

func client(t *testing.T, remote, run string, bd *fakeBD) *rig {
	t.Helper()
	dir := filepath.Join(t.TempDir(), run)
	git(t, filepath.Dir(dir), "clone", "-q", remote, dir)
	return rigAt(t, dir, run, bd)
}

func rigAt(t *testing.T, dir, run string, bd *fakeBD) *rig {
	t.Helper()
	s := openStore(t, dir, run)
	g := &flakyGit{Exec: lock.Exec{Dir: dir}}
	env := &intent.Env{BD: bd, Run: s.Run(), Store: s, Sleep: func(time.Duration) {},
		Lock: &lock.Lock{Git: g, Journal: s, Run: run, Actor: "tester"}}
	return &rig{env: env, bd: bd, store: s, git: g, dir: dir}
}

func mustClaim(t *testing.T, r *rig, id string) *intent.ClaimOut {
	t.Helper()
	out, err := intent.Claim(bg, r.env, id, false)
	if err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("claim: %+v, %v", out, err)
	}
	return out
}

func liveSHA(t *testing.T, r *rig, id string) string {
	t.Helper()
	sha, _, err := r.env.Lock.Live(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func takeOver(t *testing.T, r *rig, id string) {
	t.Helper()
	res, err := r.env.Lock.Replace(bg, id, liveSHA(t, r, id), lock.StateHeld)
	if err != nil || res.Outcome != lock.Owned {
		t.Fatalf("takeover: %+v, %v", res, err)
	}
}

func TestClaimWritesRefThenLedger(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Labels = []string{"bops:hold"}
	a := client(t, remote, "A", bd)

	out := mustClaim(t, a, "x-1")
	if !out.ReadbackOK || out.LedgerPending || out.Negative() {
		t.Fatalf("%+v", out)
	}
	b := bd.beads["x-1"]
	if b.Assignee != "tester" || b.Status != "in_progress" || !slices.Contains(b.Labels, "run:A") || slices.Contains(b.Labels, "bops:hold") {
		t.Errorf("ledger = %+v", b)
	}
	e, ok := a.store.Entry("x-1")
	if !ok || e.SHA == "" || e.SHA != liveSHA(t, a, "x-1") || e.Phase != runstate.PhaseHeld {
		t.Errorf("journal = %+v, %v", e, ok)
	}
	first, push := slices.IndexFunc(bd.calls, func(c []string) bool { return c[0] == "update" }), bd.count("dolt push")
	if first < 0 || push != 1 {
		t.Errorf("calls = %v", bd.calls)
	}
}

func TestLoserGetsTheHolderAndWritesNothing(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1") // B's replica has not seen A's claim
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")

	out, err := intent.Claim(bg, b.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeLost || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if out.Holder == nil || out.Holder.Run != "A" || out.Holder.Actor != "tester" {
		t.Errorf("holder = %+v", out.Holder)
	}
	if bdB.wrote("x-1") {
		t.Errorf("loser wrote to the ledger: %v", bdB.calls)
	}
	if _, ok := b.store.Entry("x-1"); ok {
		t.Error("loser must keep no hold")
	}
}

func TestLostThenNextOffersADifferentBead(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	bdB.add("x-2")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	if out, err := intent.Claim(bg, b.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeLost {
		t.Fatalf("%+v, %v", out, err)
	}

	next, err := intent.Next(bg, b.env, intent.NextOpts{N: 5, IncludeUnworkable: true})
	if err != nil {
		t.Fatal(err)
	}
	if next.Candidates[0].ID != "x-2" || !next.Candidates[0].Workable {
		t.Fatalf("candidates = %+v", next.Candidates)
	}
	var excluded *intent.Candidate
	for i := range next.Candidates {
		if next.Candidates[i].ID == "x-1" {
			excluded = &next.Candidates[i]
		}
	}
	if excluded == nil || excluded.Workable || !strings.Contains(strings.Join(excluded.WhyNot, ";"), "W14") {
		t.Errorf("x-1 = %+v", excluded)
	}
}

func TestRepeatClaimIsHeld(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	writes := len(bd.calls)

	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeHeld || out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") && bd.count("update x-1 --claim") != 1 {
		t.Errorf("repeat claim wrote: %v", bd.calls[writes:])
	}
}

// A crash after the push leaves an intent whose ref really is this run's.
func TestRepeatClaimAdoptsAPushThatLanded(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	other := &lock.Lock{Git: a.git, Journal: newMemJournal(), Run: "A", Actor: "tester"}
	res, err := other.Acquire(bg, "x-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Intend("x-1", res.SHA); err != nil {
		t.Fatal(err)
	}

	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeHeld || !out.LedgerPending {
		t.Fatalf("%+v, %v", out, err)
	}
	if syncOut, err := intent.Sync(bg, a.env, false); err != nil || len(syncOut.LostClaims) != 0 {
		t.Fatalf("%+v, %v", syncOut, err)
	}
	if b := bd.beads["x-1"]; b.Assignee != "tester" || len(b.Comments) != 1 {
		t.Errorf("sync should finish the ledger writes: %+v", b)
	}
}

// Two commands in one worktree can race. The loser's own run holds the bead.
func TestLosingToThisRunIsHeld(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	other := &lock.Lock{Git: a.git, Journal: newMemJournal(), Run: "A", Actor: "tester"}
	if _, err := other.Acquire(bg, "x-1"); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeHeld || out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") {
		t.Errorf("no second set of ledger writes: %v", bd.calls)
	}
}

// A note moves the claim's age forward, so working steadily is not stale.
func TestNotingKeepsAClaimFromGoingStale(t *testing.T) {
	a, b, _, _ := pair(t, "x-1")
	start := time.Now().UTC().Add(-150 * time.Minute)
	a.env.Lock.Now = func() time.Time { return start }
	mustClaim(t, a, "x-1")
	a.env.Lock.Now = func() time.Time { return start.Add(90 * time.Minute) }
	if out, err := intent.Note(bg, a.env, "x-1", "w\n\nNEXT: x"); err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatal(out, err)
	}
	out, err := intent.Claim(bg, b.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("a run that noted an hour ago must not be stale: %+v, %v", out, err)
	}
}

func TestPreconditionsRefuseBeforeAnyRef(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("assigned").Assignee = "someone"
	bd.add("labelled").Labels = []string{"run:OLD"}
	bd.add("closed").Status = "closed"
	a := client(t, remote, "A", bd)
	for _, id := range []string{"assigned", "labelled", "closed"} {
		out, err := intent.Claim(bg, a.env, id, false)
		if err != nil || out.Outcome != intent.OutcomeRefused || out.Reason == "" || !out.Negative() {
			t.Fatalf("%s: %+v, %v", id, out, err)
		}
		if liveSHA(t, a, id) != "" {
			t.Errorf("%s: no ref may exist", id)
		}
	}
	if out, _ := intent.Claim(bg, a.env, "assigned", false); !strings.Contains(out.Reason, "claim --force") {
		t.Errorf("reason should name the recovery: %q", out.Reason)
	}
}

// A shared ledger can show the winner's assignee before the ref is consulted.
func TestAssignedBeadReportsWhatTheRefSays(t *testing.T) {
	t.Run("another run holds it", func(t *testing.T) {
		a, b, _, bdB := pair(t, "x-1")
		mustClaim(t, a, "x-1")
		bdB.beads["x-1"].Assignee = "tester"
		out, err := intent.Claim(bg, b.env, "x-1", false)
		if err != nil || out.Outcome != intent.OutcomeLost || out.Holder == nil || out.Holder.Run != "A" {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("this run holds it", func(t *testing.T) {
		remote, bd := bareRemote(t), newBD()
		bd.add("x-1").Assignee = "tester"
		a := client(t, remote, "A", bd)
		other := &lock.Lock{Git: a.git, Journal: newMemJournal(), Run: "A", Actor: "tester"}
		if _, err := other.Acquire(bg, "x-1"); err != nil {
			t.Fatal(err)
		}
		out, err := intent.Claim(bg, a.env, "x-1", false)
		if err != nil || out.Outcome != intent.OutcomeHeld {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("a landing ref stays refused", func(t *testing.T) {
		a, _ := landed(t)
		out, err := intent.Claim(bg, a.env, "x-1", false)
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
	})
}

func TestPullShowingTheBeadTakenGivesTheRefBack(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	bd.onPull = func() { n.Assignee = "raw-bd-user" }
	a := client(t, remote, "A", bd)

	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("ref should be deleted")
	}
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("hold should be gone")
	}
	if bd.wrote("x-1") {
		t.Errorf("no ledger write expected: %v", bd.calls)
	}
}

func TestLedgerFailureStillClaimsAndSyncRepairsWithoutDuplicates(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	broken := true
	bd.fail = func(args []string) error {
		if broken && args[0] == "comments" && len(args) > 1 && args[1] == "add" {
			return errors.New("bd comments add: boom")
		}
		return nil
	}
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeClaimed || !out.LedgerPending || out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if e, _ := a.store.Entry("x-1"); !e.LedgerPending {
		t.Errorf("journal = %+v", e)
	}
	broken = false
	for range 2 {
		if _, err := intent.Sync(bg, a.env, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(bd.beads["x-1"].Comments); n != 1 {
		t.Errorf("comments = %d, want 1", n)
	}
	if bd.count("update x-1 --claim") != 1 {
		t.Errorf("claim repeated: %v", bd.calls)
	}
	reopened := openStore(t, a.dir, "A")
	if e, _ := reopened.Entry("x-1"); e.LedgerPending {
		t.Errorf("ledger_pending should clear: %+v", e)
	}
}

// The comment is written before the push, so a failed push must not add it twice.
func TestRetryAfterAFailedPushAddsNoSecondComment(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	broken := true
	bd.fail = func(args []string) error {
		if broken && strings.Join(args, " ") == "dolt push" {
			return errors.New("non-fast-forward")
		}
		return nil
	}
	if out := mustClaim(t, a, "x-1"); !out.LedgerPending {
		t.Fatalf("%+v", out)
	}
	broken = false
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if n := len(bd.beads["x-1"].Comments); n != 1 {
		t.Errorf("comments = %v", bd.beads["x-1"].Comments)
	}
}

func TestPushIsRetriedAfterAPull(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	rejected := 3
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt push" && rejected > 0 {
			rejected--
			return errors.New("non-fast-forward")
		}
		return nil
	}
	out := mustClaim(t, a, "x-1")
	if out.LedgerPending || bd.count("dolt push") != 4 {
		t.Errorf("%+v, pushes=%d", out, bd.count("dolt push"))
	}
}

func TestPushGivesUpAfterFiveTries(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt push" {
			return errors.New("non-fast-forward")
		}
		return nil
	}
	out := mustClaim(t, a, "x-1")
	if !out.LedgerPending || bd.count("dolt push") != 5 {
		t.Errorf("%+v, pushes=%d", out, bd.count("dolt push"))
	}
}

func TestOfflineClaimIsUnreachableWithNoLedgerWrite(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	a.git.offline = true
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if lock.CodeOf(err) != lock.CodeUnreachable || out.Outcome != intent.OutcomeUnreachable || out.Retry == "" || out.SideEffects == "" {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") {
		t.Errorf("no ledger write expected: %v", bd.calls)
	}
}

func TestRemoteCheck(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)

	bd.remotes = `[{"name":"origin","url":"git+https://github.com/other/repo.git"}]`
	if _, err := intent.Claim(bg, a.env, "x-1", false); lock.CodeOf(err) != lock.CodeRemoteMismatch {
		t.Fatalf("err = %v", err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("no ref may be written to the wrong remote")
	}
	bd.remotes = `[{"name":"origin","url":"git+file://` + remote + `"}]`
	mustClaim(t, a, "x-1")

	bd.add("x-2")
	git(t, a.dir, "remote", "remove", "origin")
	if _, err := intent.Claim(bg, a.env, "x-2", false); lock.CodeOf(err) != lock.CodeNoOrigin {
		t.Fatalf("err = %v", err)
	}
}

// With lock.remote set, the lock and the ledger check both use that remote,
// whatever origin points at.
func TestRemoteCheckUsesTheLockRemote(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	elsewhere := bareRemote(t)
	git(t, a.dir, "remote", "rename", "origin", "upstream")
	git(t, a.dir, "remote", "add", "origin", elsewhere)
	a.env.Lock.Remote = "upstream"

	// origin matches the Dolt remote, but the lock remote does not.
	bd.remotes = `[{"name":"origin","url":"git+file://` + elsewhere + `"}]`
	if _, err := intent.Claim(bg, a.env, "x-1", false); lock.CodeOf(err) != lock.CodeRemoteMismatch || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("err = %v, want remote_mismatch naming upstream", err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("no ref may be written to the wrong remote")
	}

	bd.remotes = `[{"name":"origin","url":"git+file://` + remote + `"}]`
	mustClaim(t, a, "x-1")
	if got, live := git(t, remote, "rev-parse", "refs/bops/claims/x-1"), liveSHA(t, a, "x-1"); got == "" || got != live {
		t.Errorf("ref on upstream = %q, want %q", got, live)
	}
	if left := git(t, elsewhere, "for-each-ref", "refs/bops/"); left != "" {
		t.Errorf("origin got a ref: %s", left)
	}

	bd.add("x-2")
	a.env.Lock.Remote = "mirror"
	if _, err := intent.Claim(bg, a.env, "x-2", false); lock.CodeOf(err) != lock.CodeNoOrigin || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("err = %v, want no_origin naming mirror", err)
	}
}

func TestReleaseDeletesTheRefAndWritesTheLedger(t *testing.T) {
	for _, tc := range []struct{ reason, status, label string }{
		{"blocked", "blocked", ""},
		{"human", "open", "needs:human"},
		{"scope", "open", "bops:hold"},
		{"stale", "open", "bops:hold"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			remote, bd := bareRemote(t), newBD()
			bd.add("x-1")
			a := client(t, remote, "A", bd)
			mustClaim(t, a, "x-1")
			out, err := intent.Release(bg, a.env, "x-1", tc.reason)
			if err != nil || out.Outcome != intent.OutcomeReleased || out.RefPending {
				t.Fatalf("%+v, %v", out, err)
			}
			if liveSHA(t, a, "x-1") != "" {
				t.Error("ref should be gone")
			}
			b := bd.beads["x-1"]
			if b.Status != tc.status || b.Assignee != "" || slices.Contains(b.Labels, "run:A") || (tc.label != "" && !slices.Contains(b.Labels, tc.label)) {
				t.Errorf("ledger = %+v", b)
			}
			if _, ok := a.store.Entry("x-1"); ok {
				t.Error("hold should be gone")
			}
		})
	}
}

func TestReleaseDoneKeepsTheRefAsLanding(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	if out, err := intent.Release(bg, a.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("%+v, %v", out, err)
	}
	ref, err := a.env.Lock.Read(bg, "x-1")
	if err != nil || ref == nil || ref.State != lock.StateLanding || ref.Run != "A" {
		t.Fatalf("ref = %+v, %v", ref, err)
	}
	if b := bd.beads["x-1"]; b.Status != "in_progress" || !slices.Contains(b.Labels, "bops:landing") {
		t.Errorf("ledger = %+v", b)
	}
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("hold should be gone")
	}
}

func TestLandingRewriteFailureWritesNothing(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	before := len(bd.calls)
	a.git.offline = true
	out, err := intent.Release(bg, a.env, "x-1", "done")
	if lock.CodeOf(err) != lock.CodeUnreachable || out.Outcome != intent.OutcomeUnreachable {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(bd.calls) != before {
		t.Errorf("ledger touched: %v", bd.calls[before:])
	}
	if _, ok := a.store.Entry("x-1"); !ok {
		t.Error("the hold must survive")
	}
}

func TestReleaseWithoutAHoldIsRefused(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Labels = []string{"run:A"}
	b := client(t, remote, "B", bd)
	out, err := intent.Release(bg, b.env, "x-1", "stale")
	if err != nil || out.Outcome != intent.OutcomeRefused || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.wrote("x-1") {
		t.Errorf("no write expected: %v", bd.calls)
	}
}

func TestDisplacedReleaseIsLostAndWritesNothing(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	takeOver(t, b, "x-1")
	writes := len(bdA.calls)

	out, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeLost || out.Holder == nil || out.Holder.Run != "B" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(bdA.calls) != writes {
		t.Errorf("displaced holder wrote: %v", bdA.calls[writes:])
	}
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("hold should be dropped")
	}
}

func TestLiveSiblingHoldCannotBeReleased(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	git(t, a.dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.dir, "worktree", "add", "-q", wt, "-b", "wt")
	s := rigAt(t, wt, "SIB", bd)
	mustClaim(t, s, "x-1")

	fresh := rigAt(t, a.dir, "A", bd)
	out, err := intent.Release(bg, fresh.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") == "" {
		t.Error("the live sibling's ref must stay")
	}
}

func TestStrandedHoldIsNotRefreshedAndCanBeReleased(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	git(t, a.dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.dir, "worktree", "add", "-q", wt, "-b", "wt")
	s := rigAt(t, wt, "SIB", bd)
	mustClaim(t, s, "x-1")
	git(t, a.dir, "worktree", "remove", "--force", wt)

	fresh := rigAt(t, a.dir, "A", bd)
	bd.calls = nil
	out, err := intent.Sync(bg, fresh.env, false)
	if err != nil || strings.Join(out.StrandedHolds, ",") != "x-1" || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.count("heartbeat") != 0 {
		t.Errorf("a stranded hold must not be kept alive: %v", bd.calls)
	}

	rel, err := intent.Release(bg, fresh.env, "x-1", "stale")
	if err != nil || rel.Outcome != intent.OutcomeReleased {
		t.Fatalf("%+v, %v", rel, err)
	}
	if liveSHA(t, fresh, "x-1") != "" {
		t.Error("ref should be gone")
	}
}

func TestOfflineDeleteFinishesAtSyncWithoutALostClaim(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	sha := liveSHA(t, a, "x-1")

	// The ledger check needs the remote, so only the delete goes offline.
	a.env.Lock.Git = &deleteFails{Git: a.git}
	out, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || out.Outcome != intent.OutcomeReleased || !out.RefPending {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != sha {
		t.Fatal("ref should remain")
	}

	a.env.Lock.Git = a.git
	syncOut, err := intent.Sync(bg, a.env, false)
	if err != nil || len(syncOut.LostClaims) != 0 {
		t.Fatalf("%+v, %v", syncOut, err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Error("sync should finish the delete")
	}
	if _, ok := openStore(t, a.dir, "A").Entry("x-1"); ok {
		t.Error("hold should be gone")
	}
}

type deleteFails struct{ lock.Git }

func (d *deleteFails) Run(ctx context.Context, c lock.Cmd) (lock.Output, error) {
	if c.Args[0] == "push" && strings.HasPrefix(c.Args[len(c.Args)-1], ":") {
		return lock.Output{Stderr: "fatal: unable to access remote"}, errors.New("exit status 128")
	}
	return d.Git.Run(ctx, c)
}

func TestDeleteAlreadyDoneIsNotALostClaim(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	sha := liveSHA(t, a, "x-1")
	if err := a.store.IntendDelete("x-1"); err != nil {
		t.Fatal(err)
	}
	other := &lock.Lock{Git: a.git, Journal: newMemJournal(), Run: "A", Actor: "tester"}
	if res, err := other.Delete(bg, "x-1", sha); err != nil || res.Outcome != lock.Gone {
		t.Fatalf("%+v, %v", res, err)
	}
	out, err := intent.Sync(bg, a.env, false)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestSyncReportsADisplacedHold(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	takeOver(t, b, "x-1")

	out, err := intent.Sync(bg, a.env, false)
	if err != nil || strings.Join(out.LostClaims, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if bdA.count("heartbeat") != 0 {
		t.Errorf("no heartbeat for a lost claim: %v", bdA.calls)
	}
	if _, ok := openStore(t, a.dir, "A").Entry("x-1"); ok {
		t.Error("hold should be dropped")
	}
}

func TestSyncRepairsTheDisplayFromTheRef(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")

	// A displaced run's unpublished release arrives over this holder's claim.
	n.Assignee, n.Status, n.Labels = "", "open", []string{"run:OLD", "bops:landing"}
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if n.Assignee != "tester" || n.Status != "in_progress" || !slices.Contains(n.Labels, "run:A") || slices.Contains(n.Labels, "run:OLD") || slices.Contains(n.Labels, "bops:landing") {
		t.Errorf("ledger = %+v", n)
	}

	n.Assignee, n.Status = "other", "in_progress"
	bd.calls = nil
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if n.Assignee != "tester" || bd.count("update x-1 --assignee tester --status in_progress --force") != 1 {
		t.Errorf("assignee = %q, calls = %v", n.Assignee, bd.calls)
	}

	bd.calls = nil
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if bd.wrote("x-1") {
		t.Errorf("a matching display writes nothing: %v", bd.calls)
	}
}

func TestSyncDropsAPushThatNeverLandedSilently(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd)
	if err := a.store.Intend("x-1", "1111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Sync(bg, a.env, false)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if _, ok := openStore(t, a.dir, "A").Entry("x-1"); ok {
		t.Error("entry should be gone")
	}
}

func TestSyncFailsClosedWhenOffline(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd)
	a.git.offline = true
	if _, err := intent.Sync(bg, a.env, false); lock.CodeOf(err) != lock.CodeUnreachable {
		t.Fatalf("err = %v", err)
	}
	if bd.count("dolt push") != 0 {
		t.Errorf("calls = %v", bd.calls)
	}
}

func TestEventsRecordOutcomes(t *testing.T) {
	remote := bareRemote(t)
	stateHome := os.Getenv("XDG_STATE_HOME")
	bdA := newBD()
	bdA.add("x-1")
	a := client(t, remote, "A", bdA)
	mustClaim(t, a, "x-1")
	bdB := newBD()
	bdB.add("x-1")
	b := client(t, remote, "B", bdB)
	if out, err := intent.Claim(bg, b.env, "x-1", false); err != nil || out.Outcome != intent.OutcomeLost {
		t.Fatal(out, err)
	}
	files, err := filepath.Glob(filepath.Join(stateHome, "bops", "*", "events.jsonl"))
	if err != nil || len(files) != 2 {
		t.Fatalf("files = %v, %v", files, err)
	}
	var all strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
	}
	if s := all.String(); !strings.Contains(s, `"outcome":"lost"`) || !strings.Contains(s, `"outcome":"claimed"`) {
		t.Errorf("events:\n%s", s)
	}
}

type memJournal struct{ entries map[string]runstate.Entry }

func newMemJournal() *memJournal { return &memJournal{entries: map[string]runstate.Entry{}} }

func (j *memJournal) Entry(id string) (runstate.Entry, bool) { e, ok := j.entries[id]; return e, ok }
func (j *memJournal) Intend(id, sha string) error {
	j.entries[id] = runstate.Entry{Intended: sha, Phase: runstate.PhasePushing}
	return nil
}
func (j *memJournal) Confirm(id, sha string) error {
	j.entries[id] = runstate.Entry{SHA: sha, Phase: runstate.PhaseHeld}
	return nil
}
func (j *memJournal) Abandon(id string) error      { delete(j.entries, id); return nil }
func (j *memJournal) IntendDelete(id string) error { return nil }
func (j *memJournal) Drop(id string) error         { delete(j.entries, id); return nil }

// ------------------------------------------- force, resume, note, land, sweep

const (
	prRef   = "https://github.com/o/r/pull/1"
	prSHA   = "abc1234"
	twoDays = 48 * time.Hour
)

type mergedForge struct{}

func (mergedForge) Name() string                                   { return "fake" }
func (mergedForge) IsMerged(context.Context, string) forge.Merged  { return forge.Yes }
func (mergedForge) RefSHA(context.Context, string) (string, error) { return prSHA, nil }

func later(r *rig, d time.Duration) {
	r.env.Now = func() time.Time { return time.Now().UTC().Add(d) }
}

func mustForce(t *testing.T, r *rig, id string) *intent.ClaimOut {
	t.Helper()
	out, err := intent.Claim(bg, r.env, id, true)
	if err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("force: %+v, %v", out, err)
	}
	return out
}

func pair(t *testing.T, id string) (a, b *rig, bdA, bdB *fakeBD) {
	t.Helper()
	remote := bareRemote(t)
	bdA, bdB = newBD(), newBD()
	bdA.add(id)
	bdB.add(id)
	return client(t, remote, "A", bdA), client(t, remote, "B", bdB), bdA, bdB
}

func TestForceRefusesAFreshClaim(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	bdB.beads["x-1"].Assignee = "tester"
	out, err := intent.Claim(bg, b.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeRefused || out.StaleMinutes != 120 || out.Reason == "" {
		t.Fatalf("%+v, %v", out, err)
	}
	if bdB.wrote("x-1") {
		t.Errorf("wrote: %v", bdB.calls)
	}
}

func TestForceTakesAStaleClaimAndTheOldHolderLearnsAtSync(t *testing.T) {
	a, b, bdA, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	n := bdB.beads["x-1"]
	n.Assignee, n.Status, n.Labels = "old", "in_progress", []string{"run:A"}
	later(b, 3*time.Hour)

	mustForce(t, b, "x-1")
	if n.Assignee != "tester" || !slices.Contains(n.Labels, "run:B") || slices.Contains(n.Labels, "run:A") {
		t.Errorf("ledger = %+v", n)
	}
	if !strings.Contains(strings.Join(n.Comments, ";"), "takeover=A") {
		t.Errorf("comments = %v", n.Comments)
	}
	out, err := intent.Sync(bg, a.env, false)
	if err != nil || strings.Join(out.LostClaims, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	_ = bdA
}

func TestForceRefusals(t *testing.T) {
	t.Run("landing", func(t *testing.T) {
		a, b, _, _ := pair(t, "x-1")
		mustClaim(t, a, "x-1")
		if out, err := intent.Release(bg, a.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
			t.Fatal(out, err)
		}
		later(b, twoDays)
		out, err := intent.Claim(bg, b.env, "x-1", true)
		if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Reason, "resume") {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("future time", func(t *testing.T) {
		a, b, _, _ := pair(t, "x-1")
		later(a, twoDays)
		a.env.Lock.Now = a.env.Now
		mustClaim(t, a, "x-1")
		out, err := intent.Claim(bg, b.env, "x-1", true)
		if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Reason, "clock") {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("unreadable time is not stale", func(t *testing.T) {
		remote := bareRemote(t)
		bd := newBD()
		bd.add("x-1")
		b := client(t, remote, "B", bd)
		tree := git(t, b.dir, "hash-object", "-w", "-t", "tree", "--stdin")
		commit := git(t, b.dir, "-c", "user.name=x", "-c", "user.email=x@example.invalid", "commit-tree", tree, "-m", "run=OLD\nactor=x\nstate=held\nat=yesterday")
		git(t, b.dir, "push", "-q", "origin", commit+":refs/bops/claims/x-1")
		later(b, twoDays)
		out, err := intent.Claim(bg, b.env, "x-1", true)
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("nothing to take over", func(t *testing.T) {
		_, b, _, _ := pair(t, "x-1")
		out, err := intent.Claim(bg, b.env, "x-1", true)
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
	})
}

func TestForceOnALedgerClaimWithNoRef(t *testing.T) {
	_, b, _, bd := pair(t, "x-1")
	n := bd.beads["x-1"]
	n.Assignee, n.Status = "raw-user", "in_progress"

	n.Updated = time.Now().UTC()
	out, err := intent.Claim(bg, b.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("recent: %+v, %v", out, err)
	}
	n.Updated = time.Now().UTC().Add(-3 * time.Hour)
	mustForce(t, b, "x-1")
	if n.Assignee != "tester" || liveSHA(t, b, "x-1") == "" {
		t.Errorf("ledger = %+v", n)
	}
}

func TestForceFailsClosedWhenThePullFails(t *testing.T) {
	a, b, _, bd := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	later(b, twoDays)
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("offline")
		}
		return nil
	}
	out, err := intent.Claim(bg, b.env, "x-1", true)
	if lock.CodeOf(err) != lock.CodeUnreachable || out.Outcome != intent.OutcomeUnreachable {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestConcurrentTakeoversHaveOneWinner(t *testing.T) {
	remote := bareRemote(t)
	bdA := newBD()
	bdA.add("x-1")
	a := client(t, remote, "A", bdA)
	mustClaim(t, a, "x-1")
	rigs := []*rig{}
	for _, run := range []string{"B", "C", "D"} {
		bd := newBD()
		bd.add("x-1").Assignee = "old"
		r := client(t, remote, run, bd)
		later(r, 3*time.Hour)
		rigs = append(rigs, r)
	}
	results := make([]*intent.ClaimOut, len(rigs))
	var wg sync.WaitGroup
	for i, r := range rigs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := intent.Claim(bg, r.env, "x-1", true)
			if err != nil {
				t.Errorf("%v", err)
			}
			results[i] = out
		}()
	}
	wg.Wait()
	won := 0
	for _, o := range results {
		if o != nil && o.Outcome == intent.OutcomeClaimed {
			won++
		}
	}
	if won != 1 {
		t.Errorf("winners = %d: %+v", won, results)
	}
}

func landAndFinish(t *testing.T, r *rig, id string) {
	t.Helper()
	if out, err := intent.Land(bg, r.env, id, intent.LandOpts{Ref: prRef, SHA: prSHA}); err != nil || out.Outcome != intent.OutcomeLanded {
		t.Fatalf("land: %+v, %v", out, err)
	}
	if out, err := intent.Release(bg, r.env, id, "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release: %+v, %v", out, err)
	}
}

func TestResumeTakesBackALandingBead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	landAndFinish(t, a, "x-1")

	b := client(t, remote, "B", bd)
	out, err := intent.ClaimResume(bg, b.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	ref, err := b.env.Lock.Read(bg, "x-1")
	if err != nil || ref.State != lock.StateHeld || ref.Run != "B" {
		t.Fatalf("ref = %+v, %v", ref, err)
	}
	if n.Assignee != "tester" || n.Status != "in_progress" || !slices.Contains(n.Labels, "run:B") || slices.Contains(n.Labels, "bops:landing") {
		t.Errorf("ledger = %+v", n)
	}
	if !strings.Contains(strings.Join(n.Comments, ";"), "resumed=A") {
		t.Errorf("comments = %v", n.Comments)
	}
}

func TestResumeRefusals(t *testing.T) {
	t.Run("another actor", func(t *testing.T) {
		a, b, _, _ := pair(t, "x-1")
		mustClaim(t, a, "x-1")
		if _, err := intent.Release(bg, a.env, "x-1", "done"); err != nil {
			t.Fatal(err)
		}
		b.env.Run.Actor = "someone-else"
		out, err := intent.ClaimResume(bg, b.env, "x-1")
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("held, not landing", func(t *testing.T) {
		a, b, _, _ := pair(t, "x-1")
		mustClaim(t, a, "x-1")
		out, err := intent.ClaimResume(bg, b.env, "x-1")
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("no ref", func(t *testing.T) {
		_, b, _, _ := pair(t, "x-1")
		out, err := intent.ClaimResume(bg, b.env, "x-1")
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
	})
}

// The label is display. A machine sharing the actor, whose replica still shows
// bops:landing, must not displace a holder who already resumed.
func TestSecondMachineCannotDisplaceAResumedHolder(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	if _, err := intent.Release(bg, a.env, "x-1", "done"); err != nil {
		t.Fatal(err)
	}
	if out, err := intent.ClaimResume(bg, a.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	bdB.beads["x-1"].Labels = []string{"bops:landing"}
	out, err := intent.ClaimResume(bg, b.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeRefused || out.Holder == nil || out.Holder.Run != "A" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// A landing bead stays protected when its label never reached the ledger.
func TestFailedLandingLabelStillRefusesForce(t *testing.T) {
	a, b, bdA, _ := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	bdA.fail = func(args []string) error {
		if strings.Join(args, " ") == "label add x-1 bops:landing" {
			return errors.New("boom")
		}
		return nil
	}
	rel, err := intent.Release(bg, a.env, "x-1", "done")
	if err != nil || !rel.LedgerPending {
		t.Fatalf("%+v, %v", rel, err)
	}
	later(b, twoDays)
	out, err := intent.Claim(bg, b.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestNoteRefreshesTheRef(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	before := liveSHA(t, a, "x-1")
	out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more")
	if err != nil || out.Outcome != intent.OutcomeNoted || out.CommentID == "" {
		t.Fatalf("%+v, %v", out, err)
	}
	after := liveSHA(t, a, "x-1")
	if after == before {
		t.Error("the ref should change")
	}
	if e, _ := a.store.Entry("x-1"); e.SHA != after {
		t.Errorf("journal = %+v", e)
	}
}

func TestNoteWorksWhileTheLedgerIsPending(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	n.Labels = nil // the run label never reached this ledger
	out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more")
	if err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestNoteOfflineWarnsAndStillWrites(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	comments := len(n.Comments)
	a.git.offline = true
	out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more")
	if err != nil || out.Outcome != intent.OutcomeNoted || !strings.Contains(strings.Join(out.Warnings, ";"), "not refreshed") {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(n.Comments) != comments+1 {
		t.Errorf("comments = %v", n.Comments)
	}
	if _, ok := a.store.Entry("x-1"); !ok {
		t.Error("an offline note must keep the claim")
	}
}

func TestNoteOnADisplacedHoldDropsItAndSavesTheText(t *testing.T) {
	a, b, bdA, _ := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	takeOver(t, b, "x-1")
	comments := len(bdA.beads["x-1"].Comments)
	out, err := intent.Note(bg, a.env, "x-1", "my work\n\nNEXT: more")
	if err != nil || out.Outcome != intent.OutcomeLost || !out.Negative() || out.Holder == nil || out.Holder.Run != "B" {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(bdA.beads["x-1"].Comments) != comments {
		t.Error("no comment for a lost claim")
	}
	saved, err := os.ReadFile(out.SavedTo)
	if err != nil || !strings.Contains(string(saved), "my work") {
		t.Errorf("saved = %q, %v", saved, err)
	}
	_ = os.Remove(out.SavedTo)
	if _, ok := a.store.Entry("x-1"); ok {
		t.Error("hold should be dropped")
	}
}

// A refresh whose reply was lost must not make the next note read as lost.
func TestNoteAfterALostReplyKeepsTheClaim(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	other := &lock.Lock{Git: a.git, Journal: newMemJournal(), Run: "A", Actor: "tester"}
	res, err := other.Replace(bg, "x-1", liveSHA(t, a, "x-1"), lock.StateHeld)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Intend("x-1", res.SHA); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more")
	if err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestLandDecidesOnTheRef(t *testing.T) {
	t.Run("label not needed", func(t *testing.T) {
		remote, bd := bareRemote(t), newBD()
		n := bd.add("x-1")
		a := client(t, remote, "A", bd)
		mustClaim(t, a, "x-1")
		n.Labels = nil
		out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA})
		if err != nil || out.Outcome != intent.OutcomeLanded {
			t.Fatalf("%+v, %v", out, err)
		}
	})
	t.Run("displaced", func(t *testing.T) {
		a, b, bdA, _ := pair(t, "x-1")
		mustClaim(t, a, "x-1")
		takeOver(t, b, "x-1")
		out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA})
		if err != nil || out.Outcome != intent.OutcomeLost || !out.Negative() || out.Holder == nil {
			t.Fatalf("%+v, %v", out, err)
		}
		if bdA.beads["x-1"].Metadata != nil {
			t.Error("a displaced holder must not record a landing")
		}
	})
	t.Run("offline", func(t *testing.T) {
		remote, bd := bareRemote(t), newBD()
		n := bd.add("x-1")
		a := client(t, remote, "A", bd)
		mustClaim(t, a, "x-1")
		a.git.offline = true
		out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA})
		if lock.CodeOf(err) != lock.CodeUnreachable || out.Outcome != intent.OutcomeUnreachable || n.Metadata != nil {
			t.Fatalf("%+v, %v", out, err)
		}
	})
}

func landed(t *testing.T) (*rig, *fakeBD) {
	t.Helper()
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	landAndFinish(t, a, "x-1")
	return a, bd
}

func TestSweepDeletesTheRefThenCloses(t *testing.T) {
	a, bd := landed(t)
	dry, err := intent.Sweep(bg, a.env, mergedForge{}, true)
	if err != nil || strings.Join(dry.WouldClose, ",") != "x-1" {
		t.Fatalf("%+v, %v", dry, err)
	}
	if liveSHA(t, a, "x-1") == "" || bd.beads["x-1"].Status == "closed" {
		t.Fatal("a dry run must change nothing")
	}
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Closed, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" || bd.beads["x-1"].Status != "closed" {
		t.Errorf("ref=%q status=%s", liveSHA(t, a, "x-1"), bd.beads["x-1"].Status)
	}
}

func TestSweepLeavesAResumedBead(t *testing.T) {
	a, bd := landed(t)
	if out, err := intent.ClaimResume(bg, a.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatal(out, err)
	}
	sha := liveSHA(t, a, "x-1")
	bd.beads["x-1"].Labels = []string{"bops:landing"} // a stale replica's view
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Pending, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != sha || bd.beads["x-1"].Status == "closed" {
		t.Error("a live lock must survive sweep")
	}
}

func TestSweepLeavesALandingRefFromAnotherRun(t *testing.T) {
	a, _ := landed(t)
	b := client(t, git(t, a.dir, "remote", "get-url", "origin"), "B", a.bd)
	if out, err := intent.ClaimResume(bg, b.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatal(out, err)
	}
	if out, err := intent.Release(bg, b.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatal(out, err)
	}
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Pending, ",") != "x-1" {
		t.Fatalf("the last landing is A's but the ref is B's: %+v, %v", out, err)
	}
}

// The lease decides between a sweep and a resume arriving at the same moment.
func TestSweepRacingAResumeLeavesTheHolder(t *testing.T) {
	a, bd := landed(t)
	origin := git(t, a.dir, "remote", "get-url", "origin")
	b := client(t, origin, "B", bd)
	a.git.beforePush = func() {
		if out, err := intent.ClaimResume(bg, b.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
			t.Errorf("resume: %+v, %v", out, err)
		}
	}
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Pending, ",") != "x-1" || len(out.Closed) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") == "" || bd.beads["x-1"].Status == "closed" {
		t.Error("the resumed holder must keep the bead")
	}
}

func TestSweepRecoversFromACrashBetweenDeleteAndClose(t *testing.T) {
	a, bd := landed(t)
	failing := true
	bd.fail = func(args []string) error {
		if failing && args[0] == "close" {
			return errors.New("boom")
		}
		return nil
	}
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err == nil || strings.Join(out.Unknown, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" {
		t.Fatal("the delete happened first")
	}
	failing = false
	out, err = intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Closed, ",") != "x-1" || bd.beads["x-1"].Status != "closed" {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestSweepDoesNothingWhenThePullFails(t *testing.T) {
	a, bd := landed(t)
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("offline")
		}
		return nil
	}
	if _, err := intent.Sweep(bg, a.env, mergedForge{}, false); err == nil {
		t.Fatal("want an error")
	}
	if liveSHA(t, a, "x-1") == "" || bd.beads["x-1"].Status == "closed" {
		t.Error("no deletes or closes on a failed pull")
	}
}

func TestSweepNoRefNoLabelIsPending(t *testing.T) {
	a, bd := landed(t)
	if res, err := a.env.Lock.Delete(bg, "x-1", liveSHA(t, a, "x-1")); err != nil || res.Outcome != lock.Gone {
		t.Fatal(res, err)
	}
	bd.beads["x-1"].Labels = nil
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Pending, ",") != "x-1" || bd.beads["x-1"].Status == "closed" {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestFullLoopThroughRework(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	if out, err := intent.Note(bg, a.env, "x-1", "w\n\nNEXT: x"); err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatal(out, err)
	}
	landAndFinish(t, a, "x-1")
	if out, err := intent.ClaimResume(bg, a.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatal(out, err)
	}
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef + "0", SHA: prSHA}); err != nil || out.Outcome != intent.OutcomeLanded {
		t.Fatal(out, err)
	}
	if out, err := intent.Release(bg, a.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatal(out, err)
	}
	out, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Closed, ",") != "x-1" || n.Status != "closed" || liveSHA(t, a, "x-1") != "" {
		t.Fatalf("%+v, %v, %+v", out, err, n)
	}
}
