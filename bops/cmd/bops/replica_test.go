package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
)

// The two-replica suite runs the built binary and the real bd against one bare
// git remote, the way two machines would. It needs bd on PATH and runs only
// with BOPS_INTEGRATION=1. Both replicas live on one machine, so it proves the
// behavior, not how often races happen on a real network.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("BOPS_INTEGRATION") != "1" {
		t.Skip("set BOPS_INTEGRATION=1 to run against a real bd")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Fatal("BOPS_INTEGRATION=1 but bd is not on PATH")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bops-suite-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "bops")
		if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

type replica struct {
	t     *testing.T
	dir   string
	state string
	actor string
	path  string // prepended to PATH, for stubs
}

func (r *replica) env() []string {
	env := append(os.Environ(),
		"BEADS_ACTOR="+r.actor, "XDG_STATE_HOME="+r.state, "BOPS_MACHINE="+filepath.Base(r.dir),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "BEADS_DIR=", "BEADS_DB=")
	if r.path != "" {
		env = append(env, "PATH="+r.path+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	// The suite runs bd directly as well as through bops; both must keep git
	// maintenance in the foreground, or a detached repack in the Dolt cache
	// races the next read (bops-3tm).
	return bdx.GitEnv(env)
}

func (r *replica) run(name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = r.dir, r.env()
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

func (r *replica) must(name string, args ...string) string {
	r.t.Helper()
	o, e, err := r.run(name, args...)
	if err != nil {
		r.t.Fatalf("%s %v in %s: %v\n%s\n%s", name, args, r.dir, err, o, e)
	}
	return strings.TrimSpace(o)
}

type result struct {
	Exit int
	Took time.Duration
	Out  map[string]any
	Raw  string
	Err  string
}

func (x result) outcome() string { s, _ := x.Out["outcome"].(string); return s }
func (x result) str(k string) string {
	s, _ := x.Out[k].(string)
	return s
}
func (x result) list(k string) []string {
	out := []string{}
	if xs, ok := x.Out[k].([]any); ok {
		for _, v := range xs {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func (r *replica) bopsIn(stdin string, args ...string) result {
	r.t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir, cmd.Env = r.dir, r.env()
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	start := time.Now()
	err := cmd.Run()
	x := result{Took: time.Since(start), Raw: o.String(), Err: e.String(), Out: map[string]any{}}
	if ee, ok := err.(*exec.ExitError); ok {
		x.Exit = ee.ExitCode()
	} else if err != nil {
		r.t.Errorf("bops %v: %v", args, err)
		x.Exit = -1
	}
	if jerr := json.Unmarshal(o.Bytes(), &x.Out); jerr != nil {
		r.t.Errorf("bops %v: not JSON: %v\n%s\n%s", args, jerr, o.String(), e.String())
	}
	return x
}

func (r *replica) bops(args ...string) result {
	r.t.Helper()
	return r.bopsIn("", args...)
}

type beadView struct {
	Status   string   `json:"status"`
	Assignee string   `json:"assignee"`
	Labels   []string `json:"labels"`
}

func (r *replica) show(id string) beadView {
	r.t.Helper()
	var views []beadView
	if err := json.Unmarshal([]byte(r.must("bd", "show", id, "--json")), &views); err != nil || len(views) != 1 {
		r.t.Fatalf("show %s: %v %v", id, views, err)
	}
	return views[0]
}

func (r *replica) create(title string) string {
	r.t.Helper()
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.must("bd", "create", title, "--type", "task", "--acceptance", "it works", "--json")), &got); err != nil || got.ID == "" {
		r.t.Fatalf("create: %v", err)
	}
	return got.ID
}

// refField reads a key from the claim commit's message on the remote.
func (r *replica) refField(id, key string) string {
	r.t.Helper()
	ref := "refs/bops/claims/" + id
	r.must("git", "fetch", "-q", "--no-tags", "origin", "+"+ref+":refs/suite/"+id)
	for line := range strings.SplitSeq(r.must("git", "log", "-1", "--format=%B", "refs/suite/"+id), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}

func (r *replica) refExists(id string) bool {
	r.t.Helper()
	return strings.Contains(r.must("git", "ls-remote", "origin", "refs/bops/claims/"+id), "refs/bops/claims/"+id)
}

// ageRef replaces the claim with one written three hours ago, standing in for
// a holder that went quiet. The binary has no clock to move, on purpose.
func (r *replica) ageRef(id, run, actor, state string) {
	r.t.Helper()
	at := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	tree := r.must("git", "hash-object", "-w", "-t", "tree", "--stdin")
	msg := fmt.Sprintf("run=%s\nactor=%s\nstate=%s\nnonce=%d\nat=%s\n", run, actor, state, time.Now().UnixNano(), at)
	cmd := exec.Command("git", "commit-tree", tree, "-m", msg)
	cmd.Dir, cmd.Env = r.dir, append(r.env(), "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL=x@example.invalid", "GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL=x@example.invalid")
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatal(err)
	}
	r.must("git", "push", "-q", "--no-verify", "--force", "origin", strings.TrimSpace(string(out))+":refs/bops/claims/"+id)
}

func (r *replica) identify() {
	r.must("git", "config", "user.email", "suite@example.invalid")
	r.must("git", "config", "user.name", r.actor)
}

// replicas clones one bare remote twice and shares one ledger between them.
func replicas(t *testing.T, actorA, actorB string) (a, b *replica) {
	t.Helper()
	requireLive(t)
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	seed := &replica{t: t, dir: root, state: filepath.Join(root, "state-seed"), actor: "seed"}
	seed.must("git", "init", "-q", "--bare", bare)
	a = &replica{t: t, dir: filepath.Join(root, "a"), state: filepath.Join(root, "state-a"), actor: actorA}
	b = &replica{t: t, dir: filepath.Join(root, "b"), state: filepath.Join(root, "state-b"), actor: actorB}
	seed.must("git", "clone", "-q", bare, a.dir)
	a.identify()
	a.must("git", "commit", "-q", "--allow-empty", "-m", "init")
	a.must("git", "push", "-q", "origin", "HEAD:refs/heads/main")
	a.must("bd", "init", "--quiet", "--prefix", "it")
	a.must("bd", "dolt", "push")
	seed.must("git", "clone", "-q", bare, b.dir)
	b.identify()
	b.must("bd", "bootstrap", "--yes")
	return a, b
}

// beads creates n beads on a and publishes them to b.
func beads(t *testing.T, a, b *replica, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = a.create(fmt.Sprintf("bead %d", i))
	}
	a.must("bd", "dolt", "push")
	b.must("bd", "dolt", "pull")
	return ids
}

// together runs each call at the same moment and returns results in order.
func together(calls ...func() result) []result {
	out := make([]result, len(calls))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i] = call()
		}()
	}
	close(start)
	wg.Wait()
	return out
}

func outcomes(rs []result) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = r.outcome()
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

func trials(t *testing.T) int {
	t.Helper()
	if s := os.Getenv("BOPS_RACE_TRIALS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	return 40
}

func TestReplicaRaceDistinctActors(t *testing.T) { raceTest(t, "actor-a", "actor-b") }
func TestReplicaRaceSharedActor(t *testing.T)    { raceTest(t, "same-actor", "same-actor") }

func raceTest(t *testing.T, actorA, actorB string) {
	a, b := replicas(t, actorA, actorB)
	n := trials(t)
	ids := beads(t, a, b, n)
	winners := make([]*replica, n)
	var took []time.Duration
	wins := map[*replica]int{}
	for i, id := range ids {
		rs := together(
			func() result { return a.bops("claim", id) },
			func() result { return b.bops("claim", id) },
		)
		if got := outcomes(rs); got != "claimed,lost" {
			t.Fatalf("trial %d on %s: outcomes %s\nA: %s %s\nB: %s %s", i, id, got, rs[0].Raw, rs[0].Err, rs[1].Raw, rs[1].Err)
		}
		win, lose, winner, loser := 0, 1, a, b
		if rs[1].outcome() == "claimed" {
			win, lose, winner, loser = 1, 0, b, a
		}
		winners[i] = winner
		wins[winner]++
		if rs[win].Exit != 0 || rs[win].Out["ok"] != true {
			t.Errorf("trial %d winner: exit %d %s", i, rs[win].Exit, rs[win].Raw)
		}
		if rs[lose].Exit != exitNegative || rs[lose].Out["ok"] != false {
			t.Errorf("trial %d loser: exit %d %s", i, rs[lose].Exit, rs[lose].Raw)
		}
		if h, _ := rs[lose].Out["holder"].(map[string]any); h == nil || h["run"] != rs[win].str("run") {
			t.Errorf("trial %d: loser's holder = %v, want run %s", i, rs[lose].Out["holder"], rs[win].str("run"))
		}
		if got := winner.refField(id, "run"); got != rs[win].str("run") {
			t.Errorf("trial %d: ref names %s, winner is %s", i, got, rs[win].str("run"))
		}
		if v := loser.show(id); slices.Contains(v.Labels, "run:"+rs[lose].str("run")) || v.Assignee == loser.actor && actorA != actorB {
			t.Errorf("trial %d: the loser wrote to its ledger: %+v", i, v)
		}
		took = append(took, rs[win].Took)
	}
	sort.Slice(took, func(x, y int) bool { return took[x] < took[y] })
	t.Logf("claim latency over %d trials: p50 %v, max %v; wins A=%d B=%d", n, took[n/2], took[n-1], wins[a], wins[b])

	// After both sides sync, every bead must show its winner on both ledgers.
	for _, r := range []*replica{a, b, a} {
		if x := r.bops("sync"); x.Exit != 0 || len(x.list("lost_claims")) != 0 {
			t.Fatalf("sync: exit %d %s %s", x.Exit, x.Raw, x.Err)
		}
	}
	b.must("bd", "dolt", "pull")
	for i, id := range ids {
		run := winners[i].refField(id, "run")
		for _, r := range []*replica{a, b} {
			v := r.show(id)
			if v.Assignee != winners[i].actor || v.Status != "in_progress" || !slices.Contains(v.Labels, "run:"+run) {
				t.Errorf("%s on %s: %+v, want %s run:%s", id, filepath.Base(r.dir), v, winners[i].actor, run)
			}
			if n := countPrefix(v.Labels, "run:"); n != 1 {
				t.Errorf("%s on %s: %d run labels: %v", id, filepath.Base(r.dir), n, v.Labels)
			}
		}
	}
}

func countPrefix(xs []string, p string) int {
	n := 0
	for _, x := range xs {
		if strings.HasPrefix(x, p) {
			n++
		}
	}
	return n
}

func TestReplicaDifferentBeadsBothClaim(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 6)
	for i := 0; i < len(ids); i += 2 {
		rs := together(
			func() result { return a.bops("claim", ids[i]) },
			func() result { return b.bops("claim", ids[i+1]) },
		)
		if got := outcomes(rs); got != "claimed,claimed" {
			t.Fatalf("%s\n%s\n%s", got, rs[0].Raw, rs[1].Raw)
		}
		for _, x := range rs {
			if x.Out["ledger_pending"] == true {
				t.Errorf("Dolt push contention should be retried: %s", x.Raw)
			}
		}
	}
}

func TestReplicaSameRunConcurrentClaims(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 5)
	for _, id := range ids {
		rs := together(
			func() result { return a.bops("claim", id) },
			func() result { return a.bops("claim", id) },
		)
		if got := outcomes(rs); got != "claimed,held" {
			t.Fatalf("%s: %s\n%s\n%s\n%s\n%s", id, got, rs[0].Raw, rs[0].Err, rs[1].Raw, rs[1].Err)
		}
		if n := countPrefix(a.show(id).Labels, "run:"); n != 1 {
			t.Errorf("%s: %d run labels", id, n)
		}
	}
}

func (r *replica) worktree(name string) *replica {
	r.t.Helper()
	path := filepath.Join(filepath.Dir(r.dir), name)
	r.must("git", "worktree", "add", "-q", path, "-b", name)
	return &replica{t: r.t, dir: path, state: r.state, actor: r.actor, path: r.path}
}

func TestReplicaWorktreesAreDistinctRuns(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 2)
	w1, w2 := a.worktree("w1"), a.worktree("w2")

	rs := together(
		func() result { return w1.bops("claim", ids[0]) },
		func() result { return w2.bops("claim", ids[0]) },
	)
	if got := outcomes(rs); got != "claimed,lost" {
		t.Fatalf("%s\n%s\n%s\n%s\n%s", got, rs[0].Raw, rs[0].Err, rs[1].Raw, rs[1].Err)
	}

	holder := w1
	if rs[1].outcome() == "claimed" {
		holder = w2
	}
	if x := a.bops("release", ids[0], "--reason", "scope"); x.outcome() != "refused" || x.Exit != exitNegative {
		t.Errorf("a live sibling's hold must be refused: %s", x.Raw)
	}

	a.must("git", "worktree", "remove", "--force", holder.dir)
	x := a.bops("sync")
	if !slices.Contains(x.list("stranded_holds"), ids[0]) || len(x.list("lost_claims")) != 0 {
		t.Fatalf("sync: %s", x.Raw)
	}
	if x := a.bops("release", ids[0], "--reason", "abandoned"); x.outcome() != "released" {
		t.Fatalf("stranded release: %s %s", x.Raw, x.Err)
	}
	if a.refExists(ids[0]) {
		t.Error("the ref should be gone")
	}
}

// Writing a stub that delays every push. Six worktrees at six seconds each
// would exceed the 30 second state lock wait if the lock spanned the network.
func TestReplicaManyWorktreesAgainstASlowRemote(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 6)
	stubs := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = push ]; then sleep 6; fi\nexec %q \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(stubs, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := []func() result{}
	for i, id := range ids {
		w := a.worktree(fmt.Sprintf("slow%d", i))
		w.path = stubs
		calls = append(calls, func() result { return w.bops("claim", id) })
	}
	for _, x := range together(calls...) {
		if x.outcome() != "claimed" || strings.Contains(x.Raw+x.Err, "state lock") {
			t.Errorf("exit %d: %s %s", x.Exit, x.Raw, x.Err)
		}
	}
}

func TestReplicaFailingPrePushHookDoesNotAffectClaims(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 1)
	hooks := filepath.Join(a.dir, ".git", "suite-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nsleep 2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.must("git", "config", "core.hooksPath", hooks)
	if x := a.bops("claim", ids[0]); x.outcome() != "claimed" {
		t.Fatalf("%s %s", x.Raw, x.Err)
	}
	if !a.refExists(ids[0]) {
		t.Error("the ref should exist")
	}
}

func TestReplicaTakeoverHasOneWinner(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 3)
	for _, id := range ids {
		a.ageRef(id, "OLDRUN", "gone", "held")
		rs := together(
			func() result { return a.bops("claim", id, "--force") },
			func() result { return b.bops("claim", id, "--force") },
		)
		if got := outcomes(rs); got != "claimed,lost" {
			t.Fatalf("%s\n%s\n%s\n%s\n%s", got, rs[0].Raw, rs[0].Err, rs[1].Raw, rs[1].Err)
		}
	}
}

func TestReplicaDisplacedHolderIsTold(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 2)
	for _, id := range ids {
		if x := a.bops("claim", id); x.outcome() != "claimed" {
			t.Fatal(x.Raw, x.Err)
		}
	}
	a.must("bd", "dolt", "push")
	b.must("bd", "dolt", "pull")
	for _, id := range ids {
		a.ageRef(id, "OLDRUN", "actor-a", "held")
		if x := b.bops("claim", id, "--force"); x.outcome() != "claimed" {
			t.Fatal(x.Raw, x.Err)
		}
	}

	x := a.bopsIn("my work\n\nNEXT: finish", "note", ids[0])
	if x.outcome() != "lost" || x.Exit != exitNegative || x.str("saved_to") == "" {
		t.Errorf("note: %d %s %s", x.Exit, x.Raw, x.Err)
	}
	_ = os.Remove(x.str("saved_to"))
	sync := a.bops("sync")
	if !slices.Equal(sync.list("lost_claims"), []string{ids[1]}) {
		t.Errorf("sync: %s %s", sync.Raw, sync.Err)
	}
	for _, r := range []*replica{b, a, b} {
		r.bops("sync")
	}
	if v := b.show(ids[1]); v.Assignee != "actor-b" {
		t.Errorf("display should converge on the new holder: %+v", v)
	}
}

func TestReplicaFreshClaimCannotBeForced(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 1)
	if x := a.bops("claim", ids[0]); x.outcome() != "claimed" {
		t.Fatal(x.Raw, x.Err)
	}
	before := a.must("git", "ls-remote", "origin", "refs/bops/claims/"+ids[0])
	x := b.bops("claim", ids[0], "--force")
	if x.outcome() != "refused" || x.Exit != exitNegative || x.Out["stale_minutes"] != float64(120) {
		t.Errorf("%d %s", x.Exit, x.Raw)
	}
	if got := a.must("git", "ls-remote", "origin", "refs/bops/claims/"+ids[0]); got != before {
		t.Error("a refused takeover must not move the ref")
	}
}

func TestReplicaResume(t *testing.T) {
	a, b := replicas(t, "same-actor", "same-actor")
	ids := beads(t, a, b, 2)
	id := ids[0]
	if x := a.bops("claim", id); x.outcome() != "claimed" {
		t.Fatal(x.Raw, x.Err)
	}
	if x := a.bops("land", id, "--ref", "https://github.com/o/r/pull/1", "--sha", "abc1234"); x.outcome() != "landed" {
		t.Fatal(x.Raw, x.Err)
	}
	if x := a.bops("release", id, "--reason", "done"); x.outcome() != "released" {
		t.Fatal(x.Raw, x.Err)
	}
	if got := a.refField(id, "state"); got != "landing" {
		t.Fatalf("state = %q", got)
	}
	if x := b.bops("claim", id, "--force"); x.outcome() != "refused" {
		t.Errorf("force on landing: %s", x.Raw)
	}

	other := *b
	other.actor = "someone-else"
	if x := other.bops("claim", id, "--resume"); x.outcome() != "refused" || x.Exit != exitNegative {
		t.Errorf("another actor: %d %s", x.Exit, x.Raw)
	}

	// Two machines sharing an actor: the ref state and lease allow only one.
	rs := together(
		func() result { return a.bops("claim", id, "--resume") },
		func() result { return b.bops("claim", id, "--resume") },
	)
	got := outcomes(rs)
	if got != "claimed,lost" && got != "claimed,refused" {
		t.Fatalf("%s\n%s\n%s", got, rs[0].Raw, rs[1].Raw)
	}
	if got := a.refField(id, "state"); got != "held" {
		t.Errorf("state = %q", got)
	}
}

func TestReplicaNoteKeepsTheClaimAliveAndWorksOffline(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 1)
	id := ids[0]
	if x := a.bops("claim", id); x.outcome() != "claimed" {
		t.Fatal(x.Raw, x.Err)
	}
	first := a.refField(id, "nonce")
	if x := a.bopsIn("step\n\nNEXT: more", "note", id); x.outcome() != "noted" || len(x.list("warnings")) != 0 {
		t.Fatalf("%s %s", x.Raw, x.Err)
	}
	if a.refField(id, "nonce") == first {
		t.Error("note should write a new commit to the ref")
	}

	origin := a.must("git", "remote", "get-url", "origin")
	a.must("git", "remote", "set-url", "origin", filepath.Join(filepath.Dir(a.dir), "missing.git"))
	x := a.bopsIn("offline step\n\nNEXT: more", "note", id)
	a.must("git", "remote", "set-url", "origin", origin)
	if x.outcome() != "noted" || !strings.Contains(strings.Join(x.list("warnings"), ";"), "not refreshed") {
		t.Fatalf("offline: %s %s", x.Raw, x.Err)
	}
	if h := a.bopsIn("back\n\nNEXT: more", "note", id); h.outcome() != "noted" {
		t.Errorf("after coming back: %s %s", h.Raw, h.Err)
	}
}

func TestReplicaLedgerFailureThenSyncRepairs(t *testing.T) {
	a, b := replicas(t, "actor-a", "actor-b")
	ids := beads(t, a, b, 1)
	id := ids[0]
	stubs, flag := t.TempDir(), filepath.Join(t.TempDir(), "fail")
	realBD, err := exec.LookPath("bd")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ -e %q ] && [ \"$1 $2\" = \"dolt push\" ]; then echo injected >&2; exit 1; fi\nexec %q \"$@\"\n", flag, realBD)
	if err := os.WriteFile(filepath.Join(stubs, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.path = stubs
	x := a.bops("claim", id)
	if x.outcome() != "claimed" || x.Out["ledger_pending"] != true || x.Exit != 0 {
		t.Fatalf("%d %s %s", x.Exit, x.Raw, x.Err)
	}
	if !a.refExists(id) {
		t.Fatal("the ref must exist even though the ledger push failed")
	}
	if y := b.bops("claim", id); y.outcome() != "lost" {
		t.Errorf("the other replica must lose on the ref alone: %s", y.Raw)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	if y := a.bops("sync"); y.Exit != 0 || !y.Out["pushed"].(bool) {
		t.Fatalf("%s %s", y.Raw, y.Err)
	}
	b.must("bd", "dolt", "pull")
	if v := b.show(id); v.Assignee != "actor-a" || countPrefix(v.Labels, "run:") != 1 {
		t.Errorf("display after sync: %+v", v)
	}
}

// ghStub answers every PR lookup as merged.
func ghStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\necho '{\"merged\":true}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReplicaSweep(t *testing.T) {
	a, b := replicas(t, "same-actor", "same-actor")
	ids := beads(t, a, b, 2)
	done, resumed := ids[0], ids[1]
	for i, id := range ids {
		if x := a.bops("claim", id); x.outcome() != "claimed" {
			t.Fatal(x.Raw, x.Err)
		}
		pr := fmt.Sprintf("https://github.com/o/r/pull/%d", i+1)
		if x := a.bops("land", id, "--ref", pr, "--sha", "abc1234"); x.outcome() != "landed" {
			t.Fatal(x.Raw, x.Err)
		}
		if x := a.bops("release", id, "--reason", "done"); x.outcome() != "released" {
			t.Fatal(x.Raw, x.Err)
		}
	}
	a.must("bd", "dolt", "push")
	b.must("bd", "dolt", "pull")
	if x := a.bops("claim", resumed, "--resume"); x.outcome() != "claimed" {
		t.Fatal(x.Raw, x.Err)
	}
	liveSHA := a.must("git", "ls-remote", "origin", "refs/bops/claims/"+resumed)

	// B's ledger still shows the resumed bead as landing.
	b.path = ghStub(t)
	x := b.bops("sweep", "--dry-run=false", "--forge", "github")
	if x.Exit != 0 || !slices.Equal(x.list("closed"), []string{done}) || !slices.Equal(x.list("pending"), []string{resumed}) {
		t.Fatalf("%d %s %s", x.Exit, x.Raw, x.Err)
	}
	if a.refExists(done) {
		t.Error("a merged bead's ref should be deleted")
	}
	if got := a.must("git", "ls-remote", "origin", "refs/bops/claims/"+resumed); got != liveSHA {
		t.Error("sweep must not touch a live claim")
	}
	if v := b.show(done); v.Status != "closed" {
		t.Errorf("%+v", v)
	}
}
