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
}

type fakeBD struct {
	mu      sync.Mutex
	actor   string
	beads   map[string]*bead
	remotes string
	calls   [][]string
	fail    func(args []string) error
	onPull  func()
}

func newBD() *fakeBD {
	return &fakeBD{actor: "tester", beads: map[string]*bead{}, remotes: "[]"}
}

func (b *fakeBD) add(id string) *bead {
	n := &bead{Status: "open"}
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
		return json.Marshal([]map[string]any{{"id": args[1], "status": n.Status, "assignee": n.Assignee, "labels": n.Labels,
			"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}})
	case "comments":
		if args[1] == "add" {
			b.beads[args[2]].Comments = append(b.beads[args[2]].Comments, args[3])
			return nil, nil
		}
		out := []map[string]string{}
		for _, c := range b.beads[args[1]].Comments {
			out = append(out, map[string]string{"text": c})
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
	case "ready":
		return []byte(`[{"id":"x-1","title":"t","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"},
			{"id":"x-2","title":"t","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-02T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`), nil
	case "list":
		return []byte(`[]`), nil
	}
	return nil, fmt.Errorf("fakeBD: unhandled %v", args)
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
	offline bool
}

func (g *flakyGit) Run(ctx context.Context, c lock.Cmd) (lock.Output, error) {
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
	git(t, a.dir, "commit", "-q", "--allow-empty", "-m", "init", "--author", "t <t@example.invalid>")
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
	git(t, a.dir, "commit", "-q", "--allow-empty", "-m", "init", "--author", "t <t@example.invalid>")
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
