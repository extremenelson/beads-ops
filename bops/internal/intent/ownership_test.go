package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	Notes            string
	Description      string
	Acceptance       string
	CommentTimes     []time.Time // created_at of Comments[i], when set
	Metadata         json.RawMessage
	Updated          time.Time
	Heartbeat        *time.Time
}

type fakeBD struct {
	mu      sync.Mutex
	actor   string
	beads   map[string]*bead
	remotes string // `dolt remote list --json`; "" lists none until client points it at the clone's remote
	calls   [][]string
	fail    func(args []string) error
	onPull  func()
	ready   []byte // bd ready output; nil means two fixed beads
	prime   string
}

func newBD() *fakeBD {
	return &fakeBD{actor: "tester", beads: map[string]*bead{}}
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
			if b.remotes == "" {
				return []byte("[]"), nil
			}
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
	if n.Notes != "" {
		out["notes"] = n.Notes
	}
	if n.Description != "" {
		out["description"] = n.Description
	}
	if n.Acceptance != "" {
		out["acceptance_criteria"] = n.Acceptance
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
	machine := "box" // every rig shares one machine unless clientOn names another
	if m := os.Getenv("BOPS_MACHINE"); m != "" {
		machine = m
	}
	w.Run().ID, w.Run().Actor, w.Run().Machine = run, "tester", machine
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
	afterSeen  func() // runs once, after the first read of refs/bops/seen
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
	out, err := g.Exec.Run(ctx, c)
	if c.Args[0] == "for-each-ref" && g.afterSeen != nil && strings.Contains(strings.Join(c.Args, " "), "refs/bops/seen") {
		hook := g.afterSeen
		g.afterSeen = nil
		hook()
	}
	return out, err
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
	if bd.remotes == "" { // the ledger lives beside the lock, as lock.mode ref requires
		bd.remotes = `[{"name":"origin","url":"git+file://` + remote + `"}]`
	}
	return rigAt(t, dir, run, bd)
}

func rigAt(t *testing.T, dir, run string, bd *fakeBD) *rig {
	t.Helper()
	s := openStore(t, dir, run)
	g := &flakyGit{Exec: lock.Exec{Dir: dir}}
	env := &intent.Env{BD: bd, Run: s.Run(), Store: s, Sleep: func(time.Duration) {},
		Lock: &lock.Lock{Git: g, Journal: s, Run: run, Actor: "tester", Machine: s.Run().Machine}}
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

// A blocking label refuses a plain claim before any ref, as the repo asks;
// --force, a person's takeover of a stale claim, does not read it (bops-yja).
func TestClaimRefusesABlockingLabel(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1").Labels = []string{"frozen"}
	a := client(t, remote, "A", bd)
	a.env.BlockingLabels = []string{"frozen"}
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Reason, "frozen") || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" || bd.wrote("x-1") {
		t.Errorf("a refused claim wrote a ref or the ledger: %v", bd.calls)
	}
	a.env.BlockingLabels = nil
	mustClaim(t, a, "x-1")
}

// A label added on another machine shows up only after the claim's pull, so
// the claim gives its new ref back and says why.
func TestClaimGivesBackABeadLabelledDuringThePull(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	bd.onPull = func() { n.Labels = append(n.Labels, "frozen") }
	a := client(t, remote, "A", bd)
	a.env.BlockingLabels = []string{"frozen"}
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeRefused || !strings.Contains(out.Reason, "frozen") {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != "" || bd.wrote("x-1") {
		t.Errorf("the ref must be given back with no ledger write: %v", bd.calls)
	}
}

// A bead labelled after this run claimed it still answers held to a repeat
// claim: the label keeps new work out, not work in flight (bops-yja).
func TestRepeatClaimOfALabelledBeadIsHeld(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	n.Labels = append(n.Labels, "frozen")
	a.env.BlockingLabels = []string{"frozen"}
	out, err := intent.Claim(bg, a.env, "x-1", false)
	if err != nil || out.Outcome != intent.OutcomeHeld || liveSHA(t, a, "x-1") == "" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// claim --force is a person's takeover of a stale claim: it does not read
// blocking_labels, so a frozen bead's abandoned claim can still be recovered.
func TestForceIgnoresBlockingLabels(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	n := bdB.beads["x-1"]
	n.Assignee, n.Status, n.Labels = "old", "in_progress", []string{"run:A", "frozen"}
	later(b, 3*time.Hour)
	b.env.BlockingLabels = []string{"frozen"}
	mustForce(t, b, "x-1")
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

// rejectPushes makes bd's dolt push fail non-fast-forward the first n times,
// as it does when another machine published between this one's pull and push.
func rejectPushes(bd *fakeBD, n int) {
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt push" && n > 0 {
			n--
			return errors.New("non-fast-forward")
		}
		return nil
	}
}

// Land and sync publish through pushLedger, so another machine's push between
// their pull and push costs a pull and a retry, not the command (bops-bze).
func TestLandAndSyncRetryARejectedPushAfterAPull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rejected int
		pushes   int
		ok       bool
	}{
		{"accepted on a retry", 3, 4, true},
		{"gives up after five tries", 5, 5, false},
	} {
		t.Run("land "+tc.name, func(t *testing.T) {
			remote, bd := bareRemote(t), newBD()
			bd.add("x-1")
			a := client(t, remote, "A", bd)
			mustClaim(t, a, "x-1")
			rejectPushes(bd, tc.rejected)
			pushes, pulls := bd.count("dolt push"), bd.count("dolt pull")
			out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA})
			if got := bd.count("dolt push") - pushes; got != tc.pushes {
				t.Errorf("pushes = %d, want %d", got, tc.pushes)
			}
			if got := bd.count("dolt pull") - pulls; got < tc.pushes-1 {
				t.Errorf("pulls between pushes = %d, want at least %d", got, tc.pushes-1)
			}
			if tc.ok && (err != nil || out.Outcome != intent.OutcomeLanded) {
				t.Errorf("%+v, %v", out, err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "non-fast-forward")) {
				t.Errorf("the last push error must be reported: %v", err)
			}
		})
		t.Run("sync "+tc.name, func(t *testing.T) {
			remote, bd := bareRemote(t), newBD()
			bd.add("x-1")
			a := client(t, remote, "A", bd)
			mustClaim(t, a, "x-1")
			rejectPushes(bd, tc.rejected)
			pushes := bd.count("dolt push")
			out, err := intent.Sync(bg, a.env, false)
			if got := bd.count("dolt push") - pushes; got != tc.pushes {
				t.Errorf("pushes = %d, want %d", got, tc.pushes)
			}
			if tc.ok && (err != nil || !out.Pushed) {
				t.Errorf("%+v, %v", out, err)
			}
			if !tc.ok && (err == nil || out.Pushed) {
				t.Errorf("a push that never went through must be reported: %+v, %v", out, err)
			}
		})
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

// A ledger with no Dolt remote cannot sit beside a lock remote, so under
// lock.mode ref every claim refuses before it pushes a ref or writes the
// ledger. Under lock.mode none the same ledger claims as before.
func TestNoDoltRemote(t *testing.T) {
	t.Run("ref mode refuses", func(t *testing.T) {
		remote, bd := bareRemote(t), newBD()
		n := bd.add("x-1")
		n.Status, n.Labels = "in_progress", []string{"bops:landing"}
		bd.add("x-2")
		a := client(t, remote, "A", bd)
		doltWithNoRemote(bd)
		for name, claim := range map[string]func() (*intent.ClaimOut, error){
			"claim":          func() (*intent.ClaimOut, error) { return intent.Claim(bg, a.env, "x-2", false) },
			"claim --force":  func() (*intent.ClaimOut, error) { return intent.Claim(bg, a.env, "x-2", true) },
			"claim --resume": func() (*intent.ClaimOut, error) { return intent.ClaimResume(bg, a.env, "x-1") },
		} {
			out, err := claim()
			if lock.CodeOf(err) != lock.CodeRemoteMismatch || out == nil || out.Outcome == intent.OutcomeClaimed {
				t.Fatalf("%s: %+v, %v; want remote_mismatch", name, out, err)
			}
			for _, want := range []string{"no Dolt remote", "bd dolt remote add", "lock.mode"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: %q does not say %q", name, err, want)
				}
			}
		}
		for _, id := range []string{"x-1", "x-2"} {
			if liveSHA(t, a, id) != "" || bd.wrote(id) {
				t.Errorf("%s: a refused claim wrote a ref or the ledger: %v", id, bd.calls)
			}
		}
		if left := git(t, remote, "for-each-ref", "refs/bops/"); left != "" || bd.count("dolt pull") != 0 || bd.count("dolt push") != 0 {
			t.Errorf("refs %q, calls %v", left, bd.calls)
		}
	})
	t.Run("ref mode with a matching Dolt remote claims", func(t *testing.T) {
		remote, bd := bareRemote(t), newBD()
		bd.add("x-1")
		a := client(t, remote, "A", bd)
		bd.remotes = `[{"name":"origin","url":"git+file://` + remote + `"}]`
		mustClaim(t, a, "x-1")
	})
	t.Run("ref mode claims when the remote list cannot be read", func(t *testing.T) {
		remote, bd := bareRemote(t), newBD()
		bd.add("x-1")
		a := client(t, remote, "A", bd)
		bd.fail = func(args []string) error {
			if args[0] == "dolt" && args[1] == "remote" {
				return errors.New("bd dolt remote list: exit status 1")
			}
			return nil
		}
		mustClaim(t, a, "x-1")
	})
	t.Run("none mode claims", func(t *testing.T) {
		bd := newBD()
		bd.add("x-1")
		r := lockOffRig(t, "A", bd)
		doltWithNoRemote(bd)
		out, err := intent.Claim(bg, r.env, "x-1", false)
		if err != nil || out.Outcome != intent.OutcomeClaimed {
			t.Fatalf("%+v, %v", out, err)
		}
	})
}

// saysNoDoltRemote checks that text names the missing Dolt remote and both
// fixes: add one, or set lock.mode none.
func saysNoDoltRemote(t *testing.T, what, text string) {
	t.Helper()
	for _, want := range []string{"no Dolt remote", "bd dolt remote add", "lock.mode"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s: %q does not say %q", what, text, want)
		}
	}
}

// A claim made before claims refused a missing Dolt remote, or a Dolt remote
// removed later, leaves a ref-mode hold whose ledger has nowhere to go. Sync,
// session-start and sweep name that cause instead of bd's "no remote"; land
// and release finish and warn that the ledger was not published.
func TestRefModeWithNoDoltRemoteNamesTheCause(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	mustClaim(t, a, "x-2")
	doltWithNoRemote(bd)
	calls := len(bd.calls)

	sy, err := intent.Sync(bg, a.env, false)
	if lock.CodeOf(err) != lock.CodeRemoteMismatch || sy == nil || sy.Pulled {
		t.Fatalf("sync: %+v, %v; want remote_mismatch", sy, err)
	}
	saysNoDoltRemote(t, "sync", err.Error())

	ss, err := intent.SessionStart(bg, a.env)
	if lock.CodeOf(err) != lock.CodeRemoteMismatch || ss == nil || ss.Synced {
		t.Fatalf("session-start: %+v, %v; want remote_mismatch", ss, err)
	}
	saysNoDoltRemote(t, "session-start", err.Error())

	sw, err := intent.Sweep(bg, a.env, mergedForge{}, false)
	if lock.CodeOf(err) != lock.CodeRemoteMismatch || sw == nil {
		t.Fatalf("sweep: %+v, %v; want remote_mismatch", sw, err)
	}
	saysNoDoltRemote(t, "sweep", err.Error())

	land, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA})
	if err != nil || land.Outcome != intent.OutcomeLanded || bd.beads["x-1"].Metadata == nil {
		t.Fatalf("land: %+v, %v", land, err)
	}
	saysUnpublished(t, "land", land.Warnings)
	saysNoDoltRemote(t, "land", strings.Join(land.Warnings, "\n"))

	rel, err := intent.Release(bg, a.env, "x-2", "scope")
	if err != nil || rel.Outcome != intent.OutcomeReleased || rel.RefPending {
		t.Fatalf("release: %+v, %v", rel, err)
	}
	if liveSHA(t, a, "x-2") != "" || bd.beads["x-2"].Assignee != "" {
		t.Errorf("release did not give the bead back: ref %q, assignee %q", liveSHA(t, a, "x-2"), bd.beads["x-2"].Assignee)
	}
	saysUnpublished(t, "release", rel.Warnings)
	saysNoDoltRemote(t, "release", strings.Join(rel.Warnings, "\n"))

	for _, c := range bd.calls[calls:] {
		if c[0] == "dolt" && (c[1] == "pull" || c[1] == "push") {
			t.Errorf("with no Dolt remote, ran bd %v", c)
		}
	}
}

// A Dolt remote list that cannot be read keeps sync's old behaviour: it pulls,
// and bd's own failure is the error.
func TestRefModeSyncWithAnUnreadableRemoteListPulls(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.fail = func(args []string) error {
		if args[0] == "dolt" && args[1] == "remote" {
			return errors.New("bd dolt remote list: exit status 1")
		}
		return nil
	}
	if out, err := intent.Sync(bg, a.env, false); err != nil || !out.Pulled || !out.Pushed {
		t.Fatalf("sync: %+v, %v", out, err)
	}
}

func TestReleaseDeletesTheRefAndWritesTheLedger(t *testing.T) {
	for _, tc := range []struct{ reason, status, label string }{
		{"blocked", "blocked", ""},
		{"human", "open", "needs-human"},
		{"scope", "open", "bops:hold"},
		{"abandoned", "open", "bops:hold"},
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
			if slices.Contains(b.Labels, "needs:human") {
				t.Errorf("release wrote the legacy needs:human label: %v", b.Labels)
			}
			if _, ok := a.store.Entry("x-1"); ok {
				t.Error("hold should be gone")
			}
		})
	}
}

func TestReleaseRefusesTheOldStaleReason(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	before := len(bd.calls)
	out, err := intent.Release(bg, a.env, "x-1", "stale")
	if out != nil || err == nil || !strings.Contains(err.Error(), "reason must be one of") {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(bd.calls) != before {
		t.Errorf("ledger touched: %v", bd.calls[before:])
	}
	if _, ok := a.store.Entry("x-1"); !ok {
		t.Error("the hold must survive")
	}
}

func TestReleaseDoneKeepsTheRefAsLanding(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	markLanded(a.bd.beads["x-1"])
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

// The text goes in the release's one bops:released comment, through a file,
// never as an argument.
func TestReleaseRecordsItsTextInTheReleasedComment(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	comments := len(bd.beads["x-1"].Comments)
	text := "waiting on the API key\n\nBLOCKER: secret not provisioned"
	out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", text)
	if err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("%+v, %v", out, err)
	}
	b := bd.beads["x-1"]
	if b.Status != "blocked" || len(b.Comments) != comments+1 {
		t.Fatalf("ledger = %+v", b)
	}
	if got, want := b.Comments[len(b.Comments)-1], "bops:released run=A reason=blocked op="+out.OpID+"\n"+text; got != want {
		t.Errorf("comment = %q, want %q", got, want)
	}
	for _, c := range bd.calls {
		if strings.Contains(strings.Join(c, " "), "BLOCKER") {
			t.Errorf("text on a command line: %v", c)
		}
	}
}

func TestReleaseWithoutTextKeepsTheOneLineEvent(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	out, err := intent.ReleaseWithText(bg, a.env, "x-1", "scope", "")
	if err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("%+v, %v", out, err)
	}
	c := bd.beads["x-1"].Comments
	if got := c[len(c)-1]; got != "bops:released run=A reason=scope op="+out.OpID {
		t.Errorf("comment = %q", got)
	}
}

func TestReleaseWithBlankTextWritesNothing(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	before := len(bd.calls)
	out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", " \n\t")
	if out != nil || !errors.Is(err, intent.ErrPrecondition) {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(bd.calls) != before {
		t.Errorf("ledger touched: %v", bd.calls[before:])
	}
	if _, ok := a.store.Entry("x-1"); !ok {
		t.Error("the hold must survive")
	}
}

func TestLostOrRefusedReleaseDoesNotWriteItsText(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	bdA.add("x-2")
	a, b := client(t, remote, "A", bdA), client(t, remote, "B", bdB)
	mustClaim(t, a, "x-1")
	takeOver(t, b, "x-1")
	writes := len(bdA.calls)
	if out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", "BLOCKER: x"); err != nil || out.Outcome != intent.OutcomeLost {
		t.Fatalf("lost: %+v, %v", out, err)
	}
	if out, err := intent.ReleaseWithText(bg, a.env, "x-2", "blocked", "BLOCKER: x"); err != nil || out.Outcome != intent.OutcomeRefused {
		t.Fatalf("refused: %+v, %v", out, err)
	}
	if w := writeCalls(bdA.calls[writes:]); len(w) != 0 {
		t.Errorf("wrote: %v", w)
	}
}

func TestLandingRewriteFailureWritesNothing(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	markLanded(a.bd.beads["x-1"])
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
	out, err := intent.Release(bg, b.env, "x-1", "abandoned")
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

	rel, err := intent.Release(bg, fresh.env, "x-1", "abandoned")
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

// sync prunes the lock files of beads no run holds, so the state directory
// does not keep one per bead ever touched, and keeps a held bead's (bops-exb).
func TestSyncPrunesUnheldBeadLocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps its lock files (bops-exb)")
	}
	remote := bareRemote(t)
	bd := newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	release, err := a.env.Store.LockBead("gone-1")
	if err != nil {
		t.Fatal(err)
	}
	release()
	locks := func() []string {
		m, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "beads", "*.lock"))
		return m
	}
	if n := len(locks()); n != 2 {
		t.Fatalf("want x-1's and gone-1's lock files before the sync, have %d", n)
	}

	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if n := len(locks()); n != 1 {
		t.Fatalf("sync must leave only the held bead's lock file, have %v", locks())
	}
	if release, err := a.env.Store.LockBead("x-1"); err != nil {
		t.Fatalf("the held bead's lock must still work: %v", err)
	} else {
		release()
	}
	if ev := prunedEvent(t); ev.Outcome != "pruned" || ev.Detail != "1 files" || ev.Command != "sync" {
		t.Errorf("sync must log what it pruned: %+v", ev)
	}
}

// A prune that fails costs only disk space: sync still succeeds, and the
// failure is logged (bops-exb).
func TestSyncLogsAFailedPruneAndCarriesOn(t *testing.T) {
	remote := bareRemote(t)
	bd := newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	a.env.Store = failingPrune{a.env.Store.(*runstate.Store)}

	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatalf("a failed prune must not fail the sync: %v", err)
	}
	if ev := prunedEvent(t); ev.Outcome != intent.OutcomeError || !strings.Contains(ev.Detail, "disk on fire") {
		t.Errorf("the failure must be logged: %+v", ev)
	}
}

type failingPrune struct{ *runstate.Store }

func (failingPrune) PruneBeadLocks() (int, error) { return 0, errors.New("disk on fire") }

// prunedEvent is the last bead_locks_pruned line in the clone's events.jsonl.
func prunedEvent(t *testing.T) runstate.Event {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "events.jsonl"))
	var last runstate.Event
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var ev runstate.Event
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Step == "bead_locks_pruned" {
				last = ev
			}
		}
	}
	if last.Step == "" {
		t.Fatal("no bead_locks_pruned line")
	}
	return last
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
		markLanded(a.bd.beads["x-1"])
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

// claim --resume reopens landed work for review changes; a blocking label
// added since does not stop it (bops-yja).
func TestResumeIgnoresBlockingLabels(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	n := bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	landAndFinish(t, a, "x-1")
	n.Labels = append(n.Labels, "frozen")
	b := client(t, remote, "B", bd)
	b.env.BlockingLabels = []string{"frozen"}
	if out, err := intent.ClaimResume(bg, b.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
}

func TestResumeRefusals(t *testing.T) {
	t.Run("another actor", func(t *testing.T) {
		a, b, _, _ := pair(t, "x-1")
		mustClaim(t, a, "x-1")
		markLanded(a.bd.beads["x-1"])
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

// The label is display. A session on the same machine sharing the actor, whose
// replica still shows bops:landing, must not displace a holder who already
// resumed. (From another machine, resuming a held bead is a deliberate move:
// TestResumeMovesAHeldBeadToAnotherMachine.)
func TestSecondMachineCannotDisplaceAResumedHolder(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	markLanded(a.bd.beads["x-1"])
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
	markLanded(a.bd.beads["x-1"])
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
	markLanded(b.bd.beads["x-1"])
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

// markLanded gives a fake bead the landed record `bops land` writes, so a done
// release has evidence to point at.
func markLanded(b *bead) {
	b.Metadata = json.RawMessage(`{"bops":{"landed":[{"ref":"https://github.com/o/r/pull/1","sha":"abc1234","run":"R","at":"t"}]}}`)
}

// A done release with nothing landed would leave a landing that sweep skips,
// stale ignores and next excludes, so it is refused before the ref changes.
func TestReleaseDoneWithoutALandRecordIsRefused(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	before, calls := liveSHA(t, a, "x-1"), len(bd.calls)
	out, err := intent.Release(bg, a.env, "x-1", "done")
	if err != nil || out.Outcome != intent.OutcomeRefused || !out.Negative() || !strings.Contains(out.Detail, "bops land") || !strings.Contains(out.Detail, "--reason scope") {
		t.Fatalf("%+v, %v", out, err)
	}
	ref, err := a.env.Lock.Read(bg, "x-1")
	if err != nil || ref == nil || ref.State != lock.StateHeld || ref.SHA != before {
		t.Fatalf("the ref must be untouched: %+v, %v", ref, err)
	}
	for _, c := range bd.calls[calls:] {
		if c[0] != "show" {
			t.Errorf("only a read expected, got %v", c)
		}
	}
	if _, ok := a.store.Entry("x-1"); !ok {
		t.Error("the hold must survive, so the run can land and release again")
	}
	markLanded(bd.beads["x-1"])
	if out, err := intent.Release(bg, a.env, "x-1", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("after landing: %+v, %v", out, err)
	}
}

// Metadata that cannot be read is an error, not permission to release done.
func TestReleaseDoneWithMalformedLandRecordFails(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.beads["x-1"].Metadata = json.RawMessage(`{"bops":{"landed":"nope"}}`)
	if out, err := intent.Release(bg, a.env, "x-1", "done"); err == nil || out.Outcome == intent.OutcomeReleased {
		t.Fatalf("%+v, %v", out, err)
	}
	if ref, err := a.env.Lock.Read(bg, "x-1"); err != nil || ref == nil || ref.State != lock.StateHeld {
		t.Fatalf("the ref must be untouched: %+v, %v", ref, err)
	}
}

// ------------------------------------------- concurrent sessions (bops-yph)

// writeEntry puts a journal entry straight into dir's state, as another bops
// process would have left it.
func writeEntry(t *testing.T, dir, run, id string, e runstate.Entry) {
	t.Helper()
	w, err := runstate.Open(dir, runstate.Write)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Run().ID = run
	if w.Run().Held == nil {
		w.Run().Held = runstate.Held{}
	}
	w.Run().Held[id] = e
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
}

// A second session in the same worktree, such as one opened in the main
// checkout while /bead-loop claims from it, shares the run and the clone's
// state file. Its sync read the holds when it started; if the holder moved its
// ref since (a note, a land, a release), the sync must not read the old entry
// against the new ref as a lost claim and drop the holder's hold (bops-yph).
func TestSyncInAnotherSessionKeepsAHoldReplacedSinceItsSnapshot(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	session := rigAt(t, a.dir, "A", bd) // its snapshot holds the claim's first sha

	if out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more"); err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("note: %+v, %v", out, err)
	}
	moved := liveSHA(t, a, "x-1")

	out, err := intent.Sync(bg, session.env, true)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("a claim its holder just moved is not lost: %+v, %v", out, err)
	}
	if e, ok := openStore(t, a.dir, "A").Entry("x-1"); !ok || e.SHA != moved {
		t.Fatalf("the holder's journal entry must survive: %+v, %v", e, ok)
	}
	if out, err := intent.Note(bg, a.env, "x-1", "more\n\nNEXT: land"); err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("the holder must still hold it: %+v, %v", out, err)
	}
}

// The session's snapshot holds a stale intent; before its sync reaches the
// hold, the holder confirms a write the remote has not shown this sync. The
// sync leaves it rather than settle the newer entry against the older ref.
func TestSyncDoesNotDropAWriteConfirmedAfterItsSnapshot(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	old := liveSHA(t, a, "x-1")
	writeEntry(t, a.dir, "A", "x-1", runstate.Entry{Phase: runstate.PhaseHeld, SHA: old,
		Intended: "2222222222222222222222222222222222222222"})
	session := rigAt(t, a.dir, "A", bd)
	confirmed := runstate.Entry{Phase: runstate.PhaseHeld, SHA: "3333333333333333333333333333333333333333"}
	writeEntry(t, a.dir, "A", "x-1", confirmed)

	out, err := intent.Sync(bg, session.env, true)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if e, ok := openStore(t, a.dir, "A").Entry("x-1"); !ok || e != confirmed {
		t.Fatalf("the holder's confirmed entry must stand: %+v, %v", e, ok)
	}
}

// While another bops process sharing the clone works on a bead, every other
// one leaves it alone: claim (plain, --force, --resume), note, land and
// release answer ref_contention (a note's text is kept at saved_to), sweep
// lists it as unknown, and sync defers the hold. The ref, the journal entry
// and the ledger do not change (bops-yph).
func TestCommandsLeaveABeadAnotherProcessIsWorkingOn(t *testing.T) {
	setup := func(t *testing.T) (*rig, *fakeBD, string, runstate.Entry) {
		remote, bd := bareRemote(t), newBD()
		bd.add("x-1")
		a := client(t, remote, "A", bd)
		mustClaim(t, a, "x-1")
		other := openStore(t, a.dir, "A") // another process: its own lock owner
		unlock, err := other.LockBead("x-1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unlock)
		e, _ := openStore(t, a.dir, "A").Entry("x-1")
		return rigAt(t, a.dir, "A", bd), bd, liveSHA(t, a, "x-1"), e
	}
	untouched := func(t *testing.T, r *rig, sha string, want runstate.Entry) {
		t.Helper()
		if liveSHA(t, r, "x-1") != sha {
			t.Error("the ref must stay")
		}
		if e, ok := openStore(t, r.dir, "A").Entry("x-1"); !ok || e != want {
			t.Errorf("entry = %+v, %v", e, ok)
		}
	}
	t.Run("claim", func(t *testing.T) {
		r, _, sha, want := setup(t)
		if _, err := intent.Claim(bg, r.env, "x-1", false); lock.CodeOf(err) != lock.CodeContention {
			t.Fatalf("want ref_contention, got %v", err)
		}
		untouched(t, r, sha, want)
	})
	t.Run("claim --force", func(t *testing.T) {
		r, _, sha, want := setup(t)
		out, err := intent.Claim(bg, r.env, "x-1", true)
		if lock.CodeOf(err) != lock.CodeContention || out == nil || out.Retry == "" {
			t.Fatalf("want ref_contention with retry advice, got %+v, %v", out, err)
		}
		untouched(t, r, sha, want)
	})
	t.Run("claim --resume", func(t *testing.T) {
		r, _, sha, want := setup(t)
		if _, err := intent.ClaimResume(bg, r.env, "x-1"); lock.CodeOf(err) != lock.CodeContention {
			t.Fatalf("want ref_contention, got %v", err)
		}
		untouched(t, r, sha, want)
	})
	t.Run("note", func(t *testing.T) {
		r, bd, sha, want := setup(t)
		bd.calls = nil
		out, err := intent.Note(bg, r.env, "x-1", "work\n\nNEXT: more")
		if lock.CodeOf(err) != lock.CodeContention || out == nil || out.SavedTo == "" {
			t.Fatalf("want ref_contention with the text saved, got %+v, %v", out, err)
		}
		if saved, _ := os.ReadFile(out.SavedTo); !strings.Contains(string(saved), "NEXT: more") {
			t.Errorf("saved text = %q", saved)
		}
		if bd.count("comments") != 0 || bd.count("heartbeat") != 0 {
			t.Errorf("a busy note must write nothing: %v", bd.calls)
		}
		untouched(t, r, sha, want)
	})
	t.Run("land", func(t *testing.T) {
		r, _, sha, want := setup(t)
		if _, err := intent.Land(bg, r.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA}); lock.CodeOf(err) != lock.CodeContention {
			t.Fatalf("want ref_contention, got %v", err)
		}
		untouched(t, r, sha, want)
	})
	t.Run("release", func(t *testing.T) {
		r, _, sha, want := setup(t)
		if _, err := intent.Release(bg, r.env, "x-1", "scope"); lock.CodeOf(err) != lock.CodeContention {
			t.Fatalf("want ref_contention, got %v", err)
		}
		untouched(t, r, sha, want)
	})
	t.Run("sync", func(t *testing.T) {
		r, bd, sha, want := setup(t)
		bd.calls = nil
		out, err := intent.Sync(bg, r.env, true)
		if err != nil || len(out.LostClaims) != 0 || bd.count("heartbeat") != 0 || strings.Join(out.Deferred, ",") != "x-1" {
			t.Fatalf("%+v, %v, %v", out, err, bd.calls)
		}
		untouched(t, r, sha, want)
	})
}

// sweep lists a landed bead another process is working on as unknown and
// leaves its ref and its ledger row alone (bops-yph).
func TestSweepLeavesABeadAnotherProcessIsWorkingOn(t *testing.T) {
	a, bd := landed(t)
	sha := liveSHA(t, a, "x-1")
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := intent.Sweep(bg, rigAt(t, a.dir, "A", bd).env, mergedForge{}, false)
	if err != nil || strings.Join(out.Unknown, ",") != "x-1" || len(out.Closed) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if liveSHA(t, a, "x-1") != sha || bd.beads["x-1"].Status == "closed" {
		t.Error("a bead another process is working on must survive sweep")
	}
}

// sweep reads the refs before it takes a bead's lock. Another session in the
// same run that resumes the bead in that window moves its ref; sweep must see
// that under the lock and leave the bead, not delete it or drop the
// resumer's hold (bops-yph).
func TestSweepLeavesABeadResumedAfterItReadTheRefs(t *testing.T) {
	a, bd := landed(t)
	session := rigAt(t, a.dir, "A", bd)
	session.git.afterSeen = func() {
		if out, err := intent.ClaimResume(bg, a.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
			t.Fatalf("resume: %+v, %v", out, err)
		}
		bd.beads["x-1"].Labels = []string{"bops:landing"} // a stale replica's view
	}
	out, err := intent.Sweep(bg, session.env, mergedForge{}, false)
	if err != nil || strings.Join(out.Pending, ",") != "x-1" || len(out.Closed) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	sha := liveSHA(t, a, "x-1")
	if sha == "" || bd.beads["x-1"].Status == "closed" {
		t.Fatal("the resumed claim must survive sweep")
	}
	if e, ok := openStore(t, a.dir, "A").Entry("x-1"); !ok || e.SHA != sha || e.DeletePending {
		t.Fatalf("the resumer's hold must stand: %+v, %v", e, ok)
	}
}

// A writer pushed its intent after this sync fetched the refs, then died
// before confirming it, which freed its bead lock. Settling the intent
// against the ref fetched earlier would clear it and keep the old sha while
// the remote names the new one; sync reads the live ref instead (bops-yph).
func TestSyncReadsTheLiveRefForAWriteInFlight(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	old := liveSHA(t, a, "x-1")
	if out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more"); err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("note: %+v, %v", out, err)
	}
	moved := liveSHA(t, a, "x-1")
	// Rewind to the moment before that note's push landed.
	git(t, a.dir, "push", "-q", "-f", "origin", old+":refs/bops/claims/x-1")
	writeEntry(t, a.dir, "A", "x-1", runstate.Entry{Phase: runstate.PhaseHeld, SHA: old, Intended: moved})
	session := rigAt(t, a.dir, "A", bd)
	session.git.afterSeen = func() { git(t, a.dir, "push", "-q", "-f", "origin", moved+":refs/bops/claims/x-1") }

	out, err := intent.Sync(bg, session.env, true)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if e, ok := openStore(t, a.dir, "A").Entry("x-1"); !ok || e.SHA != moved || e.Intended != "" {
		t.Fatalf("the write that landed must be adopted: %+v, %v", e, ok)
	}
}

// sweep reads the ledger before it takes a bead's lock. A bead another
// session resumed and gave back in that window has no hold and no ref left,
// but it is no longer a landing: sweep must not close it (bops-yph).
func TestSweepLeavesABeadResumedAndReleasedAfterItReadTheLedger(t *testing.T) {
	a, bd := landed(t)
	session := rigAt(t, a.dir, "A", bd)
	session.git.afterSeen = func() {
		if out, err := intent.ClaimResume(bg, a.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
			t.Fatalf("resume: %+v, %v", out, err)
		}
		if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
			t.Fatalf("release: %+v, %v", out, err)
		}
	}
	out, err := intent.Sweep(bg, session.env, mergedForge{}, false)
	if err != nil || len(out.Closed) != 0 || strings.Join(out.Pending, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if bd.beads["x-1"].Status == "closed" {
		t.Fatal("a bead given back must stay open")
	}
}

// lockStore is the real store with LockBead answering err, as a file system
// with no locks, or a broken one, would.
type lockStore struct {
	*runstate.Store
	err error
}

func (s lockStore) LockBead(string) (func(), error) { return nil, s.err }

// Where file locks are missing, commands run as before and say so once.
func TestCommandsRunUnlockedWhereFileLocksAreMissing(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	a.env.Store = lockStore{a.store, runstate.ErrBeadLockUnsupported}
	for _, id := range []string{"x-1", "x-2"} {
		out := mustClaim(t, a, id)
		if strings.Count(strings.Join(out.Warnings, "\n"), "bead lock unavailable") != 1 {
			t.Fatalf("%s: want the warning once: %q", id, out.Warnings)
		}
	}
	out, err := intent.Sync(bg, a.env, true)
	if err != nil || len(out.Deferred) != 0 || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if strings.Count(strings.Join(out.Warnings, "\n"), "bead lock unavailable") != 1 {
		t.Errorf("want the warning once for two holds: %q", out.Warnings)
	}
	if evs := lockEvents(t); len(evs) != 0 {
		t.Errorf("with no lock there is nothing to time: %+v", evs)
	}
}

// Any other failure to take the lock is reported, never read as a lost or
// finished claim: sync warns and keeps the hold, sweep returns the error.
func TestALockFailureIsReportedNotActedOn(t *testing.T) {
	a, bd := landed(t)
	broken := errors.New("state directory unreadable")
	r := rigAt(t, a.dir, "A", bd)
	r.env.Store = lockStore{r.store, broken}
	out, err := intent.Sweep(bg, r.env, mergedForge{}, false)
	if !errors.Is(err, broken) || strings.Join(out.Unknown, ",") != "x-1" || len(out.Closed) != 0 {
		t.Fatalf("sweep: %+v, %v", out, err)
	}

	remote, bd2 := bareRemote(t), newBD()
	bd2.add("x-2")
	b := client(t, remote, "B", bd2)
	mustClaim(t, b, "x-2")
	b.env.Store = lockStore{b.store, broken}
	bd2.calls = nil
	s, err := intent.Sync(bg, b.env, true)
	if err != nil || len(s.LostClaims) != 0 || bd2.count("heartbeat") != 0 || !strings.Contains(strings.Join(s.Warnings, "\n"), "x-2: state directory unreadable") {
		t.Fatalf("sync: %+v, %v", s, err)
	}
	if _, ok := openStore(t, b.dir, "B").Entry("x-2"); !ok {
		t.Fatal("the hold must stay")
	}
}

// A command waits a little for a bead another process is working on.
func TestACommandWaitsForABusyBead(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	a.env.BeadWait = time.Minute
	a.env.Sleep = func(time.Duration) { unlock() } // the other process finishes
	mustClaim(t, a, "x-1")
}

// session-start passes on the holds its sync left for later.
func TestSessionStartReportsDeferredHolds(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := intent.SessionStart(bg, rigAt(t, a.dir, "A", bd).env)
	if err != nil || strings.Join(out.Deferred, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// A landing whose hold has a write in flight is not swept.
func TestSweepLeavesABeadWithAnIntentInFlight(t *testing.T) {
	a, bd := landed(t)
	sha := liveSHA(t, a, "x-1")
	writeEntry(t, a.dir, "A", "x-1", runstate.Entry{Phase: runstate.PhaseHeld, SHA: sha, Intended: "2222222222222222222222222222222222222222"})
	out, err := intent.Sweep(bg, rigAt(t, a.dir, "A", bd).env, mergedForge{}, false)
	if err != nil || strings.Join(out.Pending, ",") != "x-1" || liveSHA(t, a, "x-1") != sha || bd.beads["x-1"].Status == "closed" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// A hold its holder released after sync read the holds is left alone.
func TestSyncLeavesAHoldReleasedAfterItsSnapshot(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	session := rigAt(t, a.dir, "A", bd)
	if out, err := intent.Release(bg, a.env, "x-1", "scope"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatal(out, err)
	}
	status := bd.beads["x-1"].Status
	out, err := intent.Sync(bg, session.env, true)
	if err != nil || len(out.LostClaims) != 0 || strings.Join(out.Deferred, ",") != "x-1" || bd.beads["x-1"].Status != status {
		t.Fatalf("%+v, %v", out, err)
	}
}

// A hold whose release never finished deleting its ref is a release in
// progress, not a claim: every command finishes the delete first, as sync
// would. claim then claims afresh; land, release and note find the bead no
// longer held, and none takes it back (bops-yph).
func TestCommandsFinishAReleaseWhoseDeleteNeverFinished(t *testing.T) {
	setup := func(t *testing.T) (*rig, *fakeBD) {
		remote, bd := bareRemote(t), newBD()
		bd.add("x-1")
		a := client(t, remote, "A", bd)
		mustClaim(t, a, "x-1")
		e, _ := openStore(t, a.dir, "A").Entry("x-1")
		e.DeletePending = true
		writeEntry(t, a.dir, "A", "x-1", e)
		// The release had already given the bead back on the ledger.
		bd.beads["x-1"].Status, bd.beads["x-1"].Assignee, bd.beads["x-1"].Labels = "open", "", nil
		return rigAt(t, a.dir, "A", bd), bd
	}
	gone := func(t *testing.T, r *rig) {
		t.Helper()
		if liveSHA(t, r, "x-1") != "" {
			t.Error("the release's delete must be finished")
		}
		if _, ok := openStore(t, r.dir, "A").Entry("x-1"); ok {
			t.Error("the hold must be gone")
		}
	}
	t.Run("claim", func(t *testing.T) {
		r, _ := setup(t)
		out := mustClaim(t, r, "x-1")
		if e, ok := openStore(t, r.dir, "A").Entry("x-1"); !ok || e.DeletePending || e.SHA != liveSHA(t, r, "x-1") || out.Outcome != intent.OutcomeClaimed {
			t.Fatalf("want a fresh claim: %+v, %+v", out, e)
		}
	})
	t.Run("land", func(t *testing.T) {
		r, _ := setup(t)
		out, err := intent.Land(bg, r.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA})
		if err != nil || out.Outcome != intent.OutcomeRefused {
			t.Fatalf("%+v, %v", out, err)
		}
		gone(t, r)
	})
	t.Run("release done", func(t *testing.T) {
		r, bd := setup(t)
		markLanded(bd.beads["x-1"])
		out, err := intent.ReleaseWithText(bg, r.env, "x-1", "done", "shipped")
		if err != nil || out.Outcome != intent.OutcomeRefused || out.SavedTo == "" {
			t.Fatalf("%+v, %v", out, err)
		}
		gone(t, r)
	})
	t.Run("note", func(t *testing.T) {
		r, _ := setup(t)
		out, err := intent.Note(bg, r.env, "x-1", "work\n\nNEXT: more")
		if err != nil || out.Outcome != intent.OutcomeRefused || out.SavedTo == "" {
			t.Fatalf("%+v, %v", out, err)
		}
		gone(t, r)
	})
}

// A release that could not run keeps its text.
func TestReleaseKeepsItsTextWhenItCannotRun(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", "waiting on the API key")
	if lock.CodeOf(err) != lock.CodeContention || out.SavedTo == "" {
		t.Fatalf("%+v, %v", out, err)
	}
	if b, _ := os.ReadFile(out.SavedTo); string(b) != "waiting on the API key" {
		t.Errorf("saved = %q", b)
	}
}

// inFlight leaves x-1 claimed by a with a note's write in flight: the journal
// intends `moved` while the remote is back at the claim's first sha.
func inFlight(t *testing.T) (a *rig, bd *fakeBD, old, moved string) {
	t.Helper()
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a = client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	old = liveSHA(t, a, "x-1")
	if out, err := intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more"); err != nil || out.Outcome != intent.OutcomeNoted {
		t.Fatalf("note: %+v, %v", out, err)
	}
	moved = liveSHA(t, a, "x-1")
	git(t, a.dir, "push", "-q", "-f", "origin", old+":refs/bops/claims/x-1")
	writeEntry(t, a.dir, "A", "x-1", runstate.Entry{Phase: runstate.PhaseHeld, SHA: old, Intended: moved})
	return a, bd, old, moved
}

// A write in flight is settled only against a ref sync could read: when it
// cannot, the hold is kept as it is and the next sync tries again.
func TestSyncKeepsAWriteInFlightWhenTheLiveReadFails(t *testing.T) {
	a, bd, old, moved := inFlight(t)
	session := rigAt(t, a.dir, "A", bd)
	session.git.afterSeen = func() { session.git.offline = true }
	bd.calls = nil
	out, err := intent.Sync(bg, session.env, true)
	if err != nil || len(out.LostClaims) != 0 || bd.count("heartbeat") != 0 || !strings.Contains(strings.Join(out.Warnings, "\n"), "x-1:") {
		t.Fatalf("%+v, %v", out, err)
	}
	if e, _ := openStore(t, a.dir, "A").Entry("x-1"); e.SHA != old || e.Intended != moved {
		t.Fatalf("the hold must be kept as it was: %+v", e)
	}
}

// A claim deleted after sync fetched is lost, read from the live ref.
func TestSyncFindsAWriteInFlightWhoseRefWasDeleted(t *testing.T) {
	a, bd, _, _ := inFlight(t)
	session := rigAt(t, a.dir, "A", bd)
	session.git.afterSeen = func() { git(t, a.dir, "push", "-q", "origin", ":refs/bops/claims/x-1") }
	out, err := intent.Sync(bg, session.env, true)
	if err != nil || strings.Join(out.LostClaims, ",") != "x-1" {
		t.Fatalf("%+v, %v", out, err)
	}
	if _, ok := openStore(t, a.dir, "A").Entry("x-1"); ok {
		t.Fatal("a lost claim's hold must go")
	}
}

// A release --reason done that pushed its landing and died before dropping
// its hold leaves a landing, not a claim: sync drops the hold rather than
// display the bead as held.
func TestSyncDropsAHoldWhoseWriteInFlightWasALanding(t *testing.T) {
	a, bd := landed(t)
	landing := git(t, a.dir, "ls-remote", "origin", "refs/bops/claims/x-1")[:40]
	if out, err := intent.ClaimResume(bg, a.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatal(out, err)
	}
	resumed := liveSHA(t, a, "x-1")
	git(t, a.dir, "push", "-q", "-f", "origin", resumed+":refs/bops/claims/x-1")
	writeEntry(t, a.dir, "A", "x-1", runstate.Entry{Phase: runstate.PhaseHeld, SHA: resumed, Intended: landing})
	session := rigAt(t, a.dir, "A", bd)
	session.git.afterSeen = func() { git(t, a.dir, "push", "-q", "-f", "origin", landing+":refs/bops/claims/x-1") }
	out, err := intent.Sync(bg, session.env, true)
	if err != nil || len(out.LostClaims) != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
	if _, ok := openStore(t, a.dir, "A").Entry("x-1"); ok {
		t.Fatal("a landing is not a hold")
	}
	if liveSHA(t, a, "x-1") != landing {
		t.Fatal("the landing must stay for sweep")
	}
}

// A command's wait for a busy bead ends: it answers ref_contention.
func TestACommandStopsWaitingForABeadThatStaysBusy(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	polls := 0
	a.env.BeadWait = 50 * time.Millisecond
	a.env.Sleep = func(time.Duration) { polls++; time.Sleep(time.Millisecond) }
	start := time.Now()
	if _, err := intent.Claim(bg, a.env, "x-1", false); lock.CodeOf(err) != lock.CodeContention || polls == 0 {
		t.Fatalf("want ref_contention after polling, got %v after %d polls", err, polls)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait did not end")
	}
}

// A release that is refused keeps its text too.
func TestAReleaseThatIsRefusedKeepsItsText(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	out, err := intent.ReleaseWithText(bg, a.env, "x-1", "blocked", "waiting on the API key")
	if err != nil || out.Outcome != intent.OutcomeRefused || out.SavedTo == "" {
		t.Fatalf("%+v, %v", out, err)
	}
}

// lockEvents reads every bead_lock event this test's clones logged.
func lockEvents(t *testing.T) []runstate.Event {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []runstate.Event
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var ev runstate.Event
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Step == "bead_lock" {
				out = append(out, ev)
			}
		}
	}
	return out
}

// Each command logs how long it held a bead's lock, and how long it waited
// for one it gave up on, so the wait can be sized from data (bops-7uu).
func TestCommandsLogHowLongTheyHeldTheBeadLock(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	bd.add("x-2")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	var released *runstate.Event
	for _, ev := range lockEvents(t) {
		if ev.Command == "claim" && ev.Bead == "x-1" && ev.Outcome == "released" {
			released = &ev
		}
	}
	if released == nil || released.WaitedMS == nil || released.HeldMS == nil {
		t.Fatalf("want claim's lock times logged, zeros included, got %+v", lockEvents(t))
	}

	unlock, err := openStore(t, a.dir, "A").LockBead("x-2")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a.env.BeadWait = 30 * time.Millisecond
	a.env.Sleep = func(time.Duration) { time.Sleep(5 * time.Millisecond) }
	if _, err := intent.Claim(bg, a.env, "x-2", false); lock.CodeOf(err) != lock.CodeContention {
		t.Fatalf("want ref_contention, got %v", err)
	}
	for _, ev := range lockEvents(t) {
		if ev.Command == "claim" && ev.Bead == "x-2" && ev.Outcome == "busy" && ev.WaitedMS != nil && *ev.WaitedMS >= 30 {
			return
		}
	}
	t.Fatalf("want the wait logged, got %+v", lockEvents(t))
}

// sync and sweep, which never wait, log a busy bead as deferred, not busy.
func TestSyncAndSweepLogABusyBeadAsDeferred(t *testing.T) {
	a, bd := landed(t) // x-1 is a landing, for sweep
	bd.add("x-2")
	mustClaim(t, a, "x-2") // a hold, for sync and session-start
	for _, id := range []string{"x-1", "x-2"} {
		unlock, err := openStore(t, a.dir, "A").LockBead(id)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
	}
	r := rigAt(t, a.dir, "A", bd)
	if _, err := intent.Sweep(bg, r.env, mergedForge{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Sync(bg, r.env, true); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.SessionStart(bg, r.env); err != nil {
		t.Fatal(err)
	}
	for _, ev := range lockEvents(t) {
		if ev.Outcome == "busy" {
			t.Fatalf("a run that does not wait must not log busy: %+v", ev)
		}
	}
	for cmd, id := range map[string]string{"sweep": "x-1", "sync": "x-2", "session-start": "x-2"} {
		found := false
		for _, ev := range lockEvents(t) {
			if ev.Command == cmd && ev.Bead == id && ev.Outcome == "deferred" {
				found = true
			}
		}
		if !found {
			t.Errorf("want %s's deferral logged, got %+v", cmd, lockEvents(t))
		}
	}
}

// A command whose time runs out while it waits for a busy bead still answers
// ref_contention, coded and retryable, and logs the cut-short wait as timeout
// rather than busy (bops-7uu).
func TestACommandOutOfTimeWhileWaitingLogsTimeout(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(bg)
	a.env.BeadWait = time.Minute
	a.env.Sleep = func(time.Duration) { cancel() }
	out, err := intent.Claim(ctx, a.env, "x-1", false)
	if lock.CodeOf(err) != lock.CodeContention || out == nil || out.Retry == "" {
		t.Fatalf("want coded ref_contention with retry advice, got %+v, %v", out, err)
	}
	var outcomes []string
	for _, ev := range lockEvents(t) {
		outcomes = append(outcomes, ev.Outcome)
	}
	if strings.Join(outcomes, ",") != "timeout" {
		t.Fatalf("want one timeout line, got %v", outcomes)
	}
}

// Each command's lock lines carry its own name (bops-7uu).
func TestLockLinesNameTheirCommand(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	unlock, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a.env.BeadWait = time.Millisecond
	a.env.Sleep = func(time.Duration) { time.Sleep(time.Millisecond) }
	runs := map[string]func(){
		"claim --force":  func() { _, _ = intent.Claim(bg, a.env, "x-1", true) },
		"claim --resume": func() { _, _ = intent.ClaimResume(bg, a.env, "x-1") },
		"note":           func() { _, _ = intent.Note(bg, a.env, "x-1", "work\n\nNEXT: more") },
		"land":           func() { _, _ = intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: prRef, SHA: prSHA}) },
		"release":        func() { _, _ = intent.Release(bg, a.env, "x-1", "scope") },
	}
	for name, run := range runs {
		run()
		found := false
		for _, ev := range lockEvents(t) {
			if ev.Command == name && ev.Outcome == "busy" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no busy line under its name in %+v", name, lockEvents(t))
		}
	}
}

// A lock taken whose state then cannot be re-read is logged as such, and
// let go (bops-7uu).
func TestALockWhoseStateCannotBeReadIsLoggedAndLetGo(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	files, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "state.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state files: %v, %v", files, err)
	}
	good, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Claim(bg, a.env, "x-1", false); err == nil {
		t.Fatal("a claim over an unreadable state file must fail")
	}
	evs := lockEvents(t)
	if len(evs) != 1 || evs[0].Outcome != "refresh_failed" || evs[0].WaitedMS == nil || evs[0].HeldMS == nil {
		t.Fatalf("want one refresh_failed line with both times, got %+v", evs)
	}
	if err := os.WriteFile(files[0], good, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := openStore(t, a.dir, "A").LockBead("x-1")
	if err != nil {
		t.Fatalf("the lock must be let go: %v", err)
	}
	again()
}

// clientOn is client on a named machine. The machine is fixed when the run is
// created (BOPS_MACHINE, else the hostname) and kept in state.json, as in use.
func clientOn(t *testing.T, remote, run string, bd *fakeBD, machine string) *rig {
	t.Helper()
	t.Setenv("BOPS_MACHINE", machine)
	return client(t, remote, run, bd)
}

// pairOn is pair with A and B on the named machines.
func pairOn(t *testing.T, id, ma, mb string) (a, b *rig, bdA, bdB *fakeBD) {
	t.Helper()
	remote := bareRemote(t)
	bdA, bdB = newBD(), newBD()
	bdA.add(id)
	bdB.add(id)
	return clientOn(t, remote, "A", bdA, ma), clientOn(t, remote, "B", bdB, mb), bdA, bdB
}

// A person carries their own held bead from laptop to desktop with claim
// --resume; the laptop finds out the way a displaced holder does (bops-xwr).
func TestResumeMovesAHeldBeadToAnotherMachine(t *testing.T) {
	a, b, _, bdB := pairOn(t, "x-1", "laptop", "desktop")
	mustClaim(t, a, "x-1")
	if ref, _ := a.env.Lock.Read(bg, "x-1"); ref == nil || ref.Machine != "laptop" {
		t.Fatalf("ref = %+v", ref)
	}
	n := bdB.beads["x-1"] // B's replica has pulled A's claim
	n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A"}

	out, err := intent.ClaimResume(bg, b.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	ref, err := b.env.Lock.Read(bg, "x-1")
	if err != nil || ref.Run != "B" || ref.State != lock.StateHeld || ref.Machine != "desktop" {
		t.Fatalf("ref = %+v, %v", ref, err)
	}
	if !slices.Contains(n.Labels, "run:B") || slices.Contains(n.Labels, "run:A") || n.Status != "in_progress" {
		t.Errorf("ledger = %+v", n)
	}
	if !strings.Contains(strings.Join(n.Comments, ";"), "resumed=A") {
		t.Errorf("comments = %v", n.Comments)
	}

	note, err := intent.Note(bg, a.env, "x-1", "late\n\nNEXT: x")
	if err != nil || note.Outcome != intent.OutcomeLost || note.SavedTo == "" {
		t.Errorf("laptop note = %+v, %v", note, err)
	}
	if again, err := intent.ClaimResume(bg, b.env, "x-1"); err != nil || again.Outcome != intent.OutcomeHeld {
		t.Errorf("second resume = %+v, %v", again, err)
	}
}

// The laptop's next sync, not only its next write, tells it the bead moved.
func TestResumedFromAnotherMachineIsALostClaim(t *testing.T) {
	a, b, _, bdB := pairOn(t, "x-1", "laptop", "desktop")
	mustClaim(t, a, "x-1")
	n := bdB.beads["x-1"]
	n.Assignee, n.Status, n.Labels = "tester", "in_progress", []string{"run:A"}
	if out, err := intent.ClaimResume(bg, b.env, "x-1"); err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	out, err := intent.Sync(bg, a.env, false)
	if err != nil || !slices.Contains(out.LostClaims, "x-1") {
		t.Fatalf("sync = %+v, %v", out, err)
	}
	if _, held := a.env.Run.Held["x-1"]; held {
		t.Error("laptop still journals the hold")
	}
}

func TestResumeOfAHeldBeadIsRefused(t *testing.T) {
	cases := []struct {
		name               string
		holder, taker      string // machines
		actor              string
		holderWritesNoMach bool
	}{
		{name: "same machine", holder: "laptop", taker: "laptop"},
		{name: "another actor", holder: "laptop", taker: "desktop", actor: "someone-else"},
		{name: "ref from an older bops", holder: "laptop", taker: "desktop", holderWritesNoMach: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b, _, _ := pairOn(t, "x-1", c.holder, c.taker)
			if c.holderWritesNoMach {
				a.env.Lock.Machine = ""
			}
			if c.actor != "" {
				b.env.Run.Actor = c.actor
			}
			mustClaim(t, a, "x-1")
			out, err := intent.ClaimResume(bg, b.env, "x-1")
			if err != nil || out.Outcome != intent.OutcomeRefused || out.Holder == nil || out.Holder.Run != "A" {
				t.Fatalf("%+v, %v", out, err)
			}
			if ref, _ := b.env.Lock.Read(bg, "x-1"); ref == nil || ref.Run != "A" {
				t.Errorf("ref moved: %+v", ref)
			}
		})
	}
}

// Two machines resuming one held bead at once. The lease lets one write win
// each round; a resume that reads the ref after the other won takes it in
// turn, as a deliberate move would. Either way the ref ends naming one run,
// and every other run that was told claimed learns at its sync that it lost.
func TestConcurrentResumesLeaveOneHolder(t *testing.T) {
	remote := bareRemote(t)
	bds := []*fakeBD{newBD(), newBD(), newBD()}
	for _, bd := range bds {
		bd.add("x-1")
	}
	a := clientOn(t, remote, "A", bds[0], "laptop")
	mustClaim(t, a, "x-1")
	takers := []*rig{clientOn(t, remote, "B", bds[1], "desktop"), clientOn(t, remote, "C", bds[2], "server")}
	outs := make([]*intent.ClaimOut, len(takers))
	var wg sync.WaitGroup
	for i, r := range takers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], _ = intent.ClaimResume(bg, r.env, "x-1")
		}()
	}
	wg.Wait()
	ref, err := a.env.Lock.Read(bg, "x-1")
	if err != nil || ref == nil || (ref.Run != "B" && ref.Run != "C") {
		t.Fatalf("ref = %+v, %v; outs %+v %+v", ref, err, outs[0], outs[1])
	}
	for i, r := range takers {
		if outs[i] == nil || r.env.Run.ID == ref.Run {
			continue
		}
		switch outs[i].Outcome {
		case intent.OutcomeLost:
		case intent.OutcomeClaimed:
			out, err := intent.Sync(bg, r.env, false)
			if err != nil || !slices.Contains(out.LostClaims, "x-1") {
				t.Errorf("%s was displaced but sync says %+v, %v", r.env.Run.ID, out, err)
			}
		default:
			t.Errorf("%s: %+v", r.env.Run.ID, outs[i])
		}
	}
}

// A claim carries generation 1 and claim --force writes 2. The holder it
// superseded cannot land, note or release: each answers lost, names the
// later generation, and writes nothing to the ledger.
func TestForcedHolderIsFencedByTheLaterGeneration(t *testing.T) {
	for _, cmd := range []string{"land", "note", "release"} {
		t.Run(cmd, func(t *testing.T) {
			a, b, bdA, _ := pair(t, "x-1")
			if out := mustClaim(t, a, "x-1"); out.Generation != 1 {
				t.Fatalf("first claim generation = %d", out.Generation)
			}
			later(b, 3*time.Hour)
			if out := mustForce(t, b, "x-1"); out.Generation != 2 {
				t.Fatalf("forced claim generation = %d", out.Generation)
			}
			bdA.calls = nil
			var outcome string
			var holder *intent.Holder
			switch cmd {
			case "land":
				out, err := intent.Land(bg, a.env, "x-1", intent.LandOpts{Ref: "https://github.com/o/r/pull/1", SHA: "abc1234"})
				if err != nil {
					t.Fatal(err)
				}
				outcome, holder = out.Outcome, out.Holder
				if len(bdA.beads["x-1"].Metadata) != 0 {
					t.Errorf("superseded holder landed: %s", bdA.beads["x-1"].Metadata)
				}
			case "note":
				out, err := intent.Note(bg, a.env, "x-1", "late work\n\nNEXT: none")
				if err != nil {
					t.Fatal(err)
				}
				outcome, holder = out.Outcome, out.Holder
				_ = os.Remove(out.SavedTo)
			case "release":
				out, err := intent.Release(bg, a.env, "x-1", "scope")
				if err != nil {
					t.Fatal(err)
				}
				outcome, holder = out.Outcome, out.Holder
			}
			if outcome != intent.OutcomeLost || holder == nil || holder.Run != "B" || holder.Generation != 2 {
				t.Fatalf("outcome = %s, holder = %+v", outcome, holder)
			}
			if bdA.wrote("x-1") {
				t.Errorf("superseded holder wrote: %v", bdA.calls)
			}
			if _, ok := a.store.Entry("x-1"); ok {
				t.Error("hold should be dropped")
			}
		})
	}
}

// claim --resume from another machine is a takeover by another run: it writes
// the next generation, and the run it displaced answers lost naming it.
func TestResumeWritesTheNextGeneration(t *testing.T) {
	remote := bareRemote(t)
	bdA, bdB := newBD(), newBD()
	bdA.add("x-1")
	bdB.add("x-1")
	a := clientOn(t, remote, "A", bdA, "laptop")
	if out := mustClaim(t, a, "x-1"); out.Generation != 1 {
		t.Fatalf("claim generation = %d", out.Generation)
	}
	b := clientOn(t, remote, "B", bdB, "desktop")
	out, err := intent.ClaimResume(bg, b.env, "x-1")
	if err != nil || out.Outcome != intent.OutcomeClaimed || out.Generation != 2 {
		t.Fatalf("resume: %+v, %v", out, err)
	}
	rel, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || rel.Outcome != intent.OutcomeLost || rel.Holder == nil || rel.Holder.Generation != 2 {
		t.Fatalf("displaced release: %+v, %v", rel, err)
	}
}
