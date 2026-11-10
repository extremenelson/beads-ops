package lock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// These tests drive real git against a bare repository. The fault wrapper sits
// on the Git seam and loses replies or changes the remote at exact moments.

var ctx = context.Background()

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func bare(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	path := filepath.Join(t.TempDir(), "remote.git")
	sh(t, filepath.Dir(path), "init", "-q", "--bare", path)
	return path
}

type fault struct {
	Exec
	before func(c Cmd) (Output, error, bool) // returns true to skip the real call
	after  func(c Cmd, out Output, err error) (Output, error)
}

func (f *fault) Run(ctx context.Context, c Cmd) (Output, error) {
	if f.before != nil {
		if out, err, skip := f.before(c); skip {
			return out, err
		}
	}
	out, err := f.Exec.Run(ctx, c)
	if f.after != nil {
		return f.after(c, out, err)
	}
	return out, err
}

type memJournal struct {
	entries map[string]runstate.Entry
	fail    map[string]error
}

func newJournal() *memJournal {
	return &memJournal{entries: map[string]runstate.Entry{}, fail: map[string]error{}}
}

func (j *memJournal) Entry(id string) (runstate.Entry, bool) { e, ok := j.entries[id]; return e, ok }
func (j *memJournal) Intend(id, sha string) error {
	if err := j.fail["intend"]; err != nil {
		return err
	}
	e, ok := j.entries[id]
	if !ok {
		e.Phase = runstate.PhasePushing
	}
	e.Intended = sha
	j.entries[id] = e
	return nil
}
func (j *memJournal) Confirm(id, sha string) error {
	if err := j.fail["confirm"]; err != nil {
		return err
	}
	e := j.entries[id]
	e.SHA, e.Intended, e.Phase = sha, "", runstate.PhaseHeld
	j.entries[id] = e
	return nil
}
func (j *memJournal) Abandon(id string) error {
	e, ok := j.entries[id]
	if !ok {
		return nil
	}
	if e.Phase == runstate.PhasePushing {
		delete(j.entries, id)
		return nil
	}
	e.Intended = ""
	j.entries[id] = e
	return nil
}
func (j *memJournal) IntendDelete(id string) error {
	if e, ok := j.entries[id]; ok {
		e.DeletePending = true
		j.entries[id] = e
	}
	return nil
}
func (j *memJournal) Drop(id string) error { delete(j.entries, id); return nil }

type side struct {
	lock *Lock
	git  *fault
	jr   *memJournal
	dir  string
}

func client(t *testing.T, remote, run string) *side {
	t.Helper()
	dir := filepath.Join(t.TempDir(), run)
	sh(t, filepath.Dir(dir), "clone", "-q", remote, dir)
	f := &fault{Exec: Exec{Dir: dir}}
	j := newJournal()
	return &side{lock: &Lock{Git: f, Journal: j, Run: run, Actor: "actor-" + run}, git: f, jr: j, dir: dir}
}

func acquire(t *testing.T, s *side, id string) string {
	t.Helper()
	res, err := s.lock.Acquire(ctx, id)
	if err != nil || res.Outcome != Owned {
		t.Fatalf("acquire: %+v, %v", res, err)
	}
	return res.SHA
}

func isPush(c Cmd) bool { return c.Args[0] == "push" }

// loseReply applies the next push and then pretends the network died.
func loseReply(s *side) {
	s.git.after = func(c Cmd, out Output, err error) (Output, error) {
		if isPush(c) {
			s.git.after = nil
			return Output{Stderr: "fatal: the remote end hung up unexpectedly"}, errors.New("exit status 128")
		}
		return out, err
	}
}

// dropNetwork fails every push before it leaves the machine.
func dropPushes(s *side) {
	s.git.before = func(c Cmd) (Output, error, bool) {
		return Output{Stderr: "fatal: unable to access remote"}, errors.New("exit status 128"), isPush(c)
	}
}

func TestAcquireWritesTheMessage(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	a.lock.Now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	sha := acquire(t, a, "bops-1.2")

	if e := a.jr.entries["bops-1.2"]; e.SHA != sha || e.Phase != runstate.PhaseHeld || e.Intended != "" {
		t.Errorf("journal = %+v", e)
	}
	ref, err := a.lock.Read(ctx, "bops-1.2")
	if err != nil || ref == nil {
		t.Fatalf("read: %v, %v", ref, err)
	}
	if ref.Run != "A" || ref.Actor != "actor-A" || ref.State != StateHeld || ref.Nonce == "" || ref.SHA != sha || ref.At.Year() != 2026 {
		t.Errorf("ref = %+v", ref)
	}
	if got := sh(t, a.dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>", sha); got != "bops <bops@invalid>|bops <bops@invalid>" {
		t.Errorf("identity = %q", got)
	}
}

func TestSecondAcquireLosesAndNamesTheHolder(t *testing.T) {
	r := bare(t)
	a, b := client(t, r, "A"), client(t, r, "B")
	sha := acquire(t, a, "x-1")

	res, err := b.lock.Acquire(ctx, "x-1")
	if err != nil || res.Outcome != Lost {
		t.Fatalf("got %+v, %v", res, err)
	}
	if res.Holder == nil || res.Holder.Run != "A" || res.Holder.SHA != sha {
		t.Errorf("holder = %+v", res.Holder)
	}
	if _, ok := b.jr.entries["x-1"]; ok {
		t.Error("a loser keeps no journal entry")
	}
	seen, err := b.lock.Seen(ctx)
	if err != nil || len(seen) != 1 || seen[0].ID != "x-1" {
		t.Errorf("seen = %+v, %v", seen, err)
	}
}

// A lost reply on any write must not read as a loss of the bead.
func TestLostReplyIsAdopted(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")

	loseReply(a)
	res, err := a.lock.Acquire(ctx, "x-1")
	if err != nil || res.Outcome != Owned {
		t.Fatalf("create: %+v, %v", res, err)
	}
	first := res.SHA

	loseReply(a)
	res, err = a.lock.Replace(ctx, "x-1", first, StateHeld)
	if err != nil || res.Outcome != Owned || res.SHA == first {
		t.Fatalf("refresh: %+v, %v", res, err)
	}

	loseReply(a)
	landing, err := a.lock.Replace(ctx, "x-1", res.SHA, StateLanding)
	if err != nil || landing.Outcome != Owned {
		t.Fatalf("landing: %+v, %v", landing, err)
	}

	b := client(t, r, "B") // takeover from another clone, as force and resume do
	loseReply(b)
	took, err := b.lock.Replace(ctx, "x-1", landing.SHA, StateHeld)
	if err != nil || took.Outcome != Owned {
		t.Fatalf("takeover: %+v, %v", took, err)
	}

	loseReply(b)
	gone, err := b.lock.Delete(ctx, "x-1", took.SHA)
	if err != nil || gone.Outcome != Gone {
		t.Fatalf("delete: %+v, %v", gone, err)
	}
	if _, found, _ := b.lock.Live(ctx, "x-1"); found {
		t.Error("ref should be gone")
	}
}

// A failed refresh with the ref unchanged is a network problem. Calling it
// lost would drop a bead the run still owns.
func TestFailedRefreshIsNotLost(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	sha := acquire(t, a, "x-1")
	dropPushes(a)

	res, err := a.lock.Replace(ctx, "x-1", sha, StateHeld)
	if CodeOf(err) != CodeUnreachable {
		t.Fatalf("got %+v, %v", res, err)
	}
	e := a.jr.entries["x-1"]
	if e.SHA != sha || e.Intended == "" {
		t.Errorf("journal should keep the claim and the intent: %+v", e)
	}
	out, err := a.lock.Settle("x-1", sha)
	if err != nil || out != Kept {
		t.Errorf("settle = %v, %v", out, err)
	}
	if e := a.jr.entries["x-1"]; e.Intended != "" || e.SHA != sha {
		t.Errorf("after settle: %+v", e)
	}
}

func TestReplaceWithStaleLeaseIsLost(t *testing.T) {
	r := bare(t)
	a, b := client(t, r, "A"), client(t, r, "B")
	sha := acquire(t, a, "x-1")
	if res, err := b.lock.Replace(ctx, "x-1", sha, StateHeld); err != nil || res.Outcome != Owned {
		t.Fatalf("takeover: %+v, %v", res, err)
	}
	res, err := a.lock.Replace(ctx, "x-1", sha, StateHeld)
	if err != nil || res.Outcome != Lost || res.Holder == nil || res.Holder.Run != "B" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if a.jr.entries["x-1"].Intended != "" {
		t.Error("intent should be cleared")
	}
}

func TestReplaceOfDeletedRefIsLost(t *testing.T) {
	r := bare(t)
	a, b := client(t, r, "A"), client(t, r, "B")
	sha := acquire(t, a, "x-1")
	if res, err := b.lock.Delete(ctx, "x-1", sha); err != nil || res.Outcome != Gone {
		t.Fatalf("delete: %+v, %v", res, err)
	}
	res, err := a.lock.Replace(ctx, "x-1", sha, StateHeld)
	if err != nil || res.Outcome != Lost || res.Holder != nil {
		t.Fatalf("got %+v, %v", res, err)
	}
}

// A holder releasing between the rejection and the re-read looks exactly like
// a forge that refuses custom refs, until the create is tried once more.
func TestReleaseRacingCreateRetries(t *testing.T) {
	r := bare(t)
	a, b := client(t, r, "A"), client(t, r, "B")
	sha := acquire(t, a, "x-1")
	b.git.after = func(c Cmd, out Output, err error) (Output, error) {
		if isPush(c) {
			b.git.after = nil
			if res, derr := a.lock.Delete(ctx, "x-1", sha); derr != nil || res.Outcome != Gone {
				t.Fatalf("delete: %+v, %v", res, derr)
			}
		}
		return out, err
	}
	res, err := b.lock.Acquire(ctx, "x-1")
	if err != nil || res.Outcome != Owned {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestForgeRefusingRefsIsLockUnsupported(t *testing.T) {
	r := bare(t)
	hook := filepath.Join(r, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := client(t, r, "A")
	res, err := a.lock.Acquire(ctx, "x-1")
	if CodeOf(err) != CodeLockUnsupported {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, ok := a.jr.entries["x-1"]; ok {
		t.Error("nothing should stay journaled")
	}
}

func TestUnreachableKeepsTheIntent(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	a.git.before = func(c Cmd) (Output, error, bool) {
		if isPush(c) || c.Args[0] == "ls-remote" {
			return Output{Stderr: "fatal: unable to access 'https://user:tok3n@example.invalid/r.git/'"}, errors.New("exit status 128"), true
		}
		return Output{}, nil, false
	}
	_, err := a.lock.Acquire(ctx, "x-1")
	if CodeOf(err) != CodeUnreachable {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "tok3n") {
		t.Errorf("credentials leaked: %v", err)
	}
	if a.jr.entries["x-1"].Intended == "" {
		t.Error("an unknown outcome must stay journaled")
	}
}

func TestAuthFailureHasItsOwnCode(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	a.git.before = func(c Cmd) (Output, error, bool) {
		if isPush(c) {
			return Output{Stderr: "fatal: could not read Username for 'https://github.com': terminal prompts disabled"}, errors.New("exit status 128"), true
		}
		return Output{}, nil, false
	}
	if _, err := a.lock.Acquire(ctx, "x-1"); CodeOf(err) != CodeAuth {
		t.Fatalf("err = %v", err)
	}
}

func TestJournalFailureBeforePushWritesNoRef(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	a.jr.fail["intend"] = errors.New("disk full")
	if _, err := a.lock.Acquire(ctx, "x-1"); CodeOf(err) != CodeJournal {
		t.Fatalf("err = %v", err)
	}
	if _, found, _ := a.lock.Live(ctx, "x-1"); found {
		t.Error("no ref may be pushed without an intent")
	}
}

func TestHoldNotRecordedTakesTheRefBack(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	a.jr.fail["confirm"] = errors.New("disk full")
	_, err := a.lock.Acquire(ctx, "x-1")
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeHoldNotRecorded || e.SHA != "" {
		t.Fatalf("err = %#v", err)
	}
	if _, found, _ := a.lock.Live(ctx, "x-1"); found {
		t.Error("the ref should have been deleted")
	}
}

func TestHoldNotRecordedReportsTheShaIfDeleteFails(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	a.jr.fail["confirm"] = errors.New("disk full")
	pushes := 0
	a.git.before = func(c Cmd) (Output, error, bool) {
		if isPush(c) {
			if pushes++; pushes > 1 {
				return Output{}, errors.New("exit status 128"), true
			}
		}
		return Output{}, nil, false
	}
	_, err := a.lock.Acquire(ctx, "x-1")
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeHoldNotRecorded || e.SHA == "" {
		t.Fatalf("err = %#v", err)
	}
}

func TestDeleteOutcomes(t *testing.T) {
	r := bare(t)
	a, b := client(t, r, "A"), client(t, r, "B")

	sha := acquire(t, a, "gone")
	if res, err := b.lock.Delete(ctx, "gone", sha); err != nil || res.Outcome != Gone {
		t.Fatalf("first delete: %+v, %v", res, err)
	}
	if res, err := a.lock.Delete(ctx, "gone", sha); err != nil || res.Outcome != Gone {
		t.Fatalf("already gone: %+v, %v", res, err)
	}
	if _, ok := a.jr.entries["gone"]; ok {
		t.Error("hold should be dropped")
	}

	old := acquire(t, a, "moved")
	if res, err := b.lock.Replace(ctx, "moved", old, StateHeld); err != nil || res.Outcome != Owned {
		t.Fatalf("takeover: %+v, %v", res, err)
	}
	if res, err := a.lock.Delete(ctx, "moved", old); err != nil || res.Outcome != Lost {
		t.Fatalf("stale delete: %+v, %v", res, err)
	}
	if _, found, _ := a.lock.Live(ctx, "moved"); !found {
		t.Error("a stale lease must not delete the ref")
	}

	stuck := acquire(t, a, "stuck")
	dropPushes(a)
	if _, err := a.lock.Delete(ctx, "stuck", stuck); CodeOf(err) != CodeUnreachable {
		t.Fatalf("offline delete: %v", err)
	}
	if !a.jr.entries["stuck"].DeletePending {
		t.Error("delete_pending should be set for sync")
	}
}

func TestSettle(t *testing.T) {
	cases := []struct {
		name  string
		entry *runstate.Entry
		live  string
		want  Reconcile
		left  bool
	}{
		{"none", nil, "", Discarded, false},
		{"pushing landed", &runstate.Entry{Intended: "b", Phase: runstate.PhasePushing}, "b", Adopted, true},
		{"pushing never landed", &runstate.Entry{Intended: "b", Phase: runstate.PhasePushing}, "", Discarded, false},
		{"pushing beaten", &runstate.Entry{Intended: "b", Phase: runstate.PhasePushing}, "c", Discarded, false},
		{"refresh landed", &runstate.Entry{SHA: "a", Intended: "b", Phase: runstate.PhaseHeld}, "b", Adopted, true},
		{"refresh missed", &runstate.Entry{SHA: "a", Intended: "b", Phase: runstate.PhaseHeld}, "a", Kept, true},
		{"refresh displaced", &runstate.Entry{SHA: "a", Intended: "b", Phase: runstate.PhaseHeld}, "c", Displaced, true},
		{"held matches", &runstate.Entry{SHA: "a", Phase: runstate.PhaseHeld}, "a", Unchanged, true},
		{"held displaced", &runstate.Entry{SHA: "a", Phase: runstate.PhaseHeld}, "c", Displaced, true},
		{"held and gone", &runstate.Entry{SHA: "a", Phase: runstate.PhaseHeld}, "", Displaced, true},
		{"legacy is left alone", &runstate.Entry{Phase: runstate.PhaseHeld}, "", Unchanged, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newJournal()
			if c.entry != nil {
				j.entries["x"] = *c.entry
			}
			l := &Lock{Journal: j}
			got, err := l.Settle("x", c.live)
			if err != nil || got != c.want {
				t.Fatalf("got %v, %v, want %v", got, err, c.want)
			}
			if _, ok := j.entries["x"]; ok != c.left {
				t.Errorf("entry kept = %v, want %v", ok, c.left)
			}
		})
	}
}

func TestFetchAndSeen(t *testing.T) {
	r := bare(t)
	a, b := client(t, r, "A"), client(t, r, "B")
	if err := b.lock.Fetch(ctx); err != nil {
		t.Fatalf("fetch of an empty namespace: %v", err)
	}
	one := acquire(t, a, "x-1")
	acquire(t, a, "x-2")
	if err := b.lock.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	seen, err := b.lock.Seen(ctx)
	if err != nil || len(seen) != 2 || seen[0].Run != "A" || seen[0].State != StateHeld {
		t.Fatalf("seen = %+v, %v", seen, err)
	}
	if res, err := a.lock.Delete(ctx, "x-1", one); err != nil || res.Outcome != Gone {
		t.Fatal(res, err)
	}
	if err := b.lock.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if seen, _ := b.lock.Seen(ctx); len(seen) != 1 || seen[0].ID != "x-2" {
		t.Errorf("prune failed: %+v", seen)
	}
}

func TestReadOfAbsentRefIsNil(t *testing.T) {
	a := client(t, bare(t), "A")
	if ref, err := a.lock.Read(ctx, "x-1"); err != nil || ref != nil {
		t.Errorf("got %+v, %v", ref, err)
	}
}

func TestLocalContentionIsNotUnreachable(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	acquire(t, a, "x-1")
	a.git.before = func(c Cmd) (Output, error, bool) {
		if c.Args[0] == "fetch" {
			return Output{Stderr: "error: cannot lock ref 'refs/bops/seen/x-1': is at a but expected b"}, errors.New("exit status 1"), true
		}
		return Output{}, nil, false
	}
	if _, err := a.lock.Read(ctx, "x-1"); CodeOf(err) != CodeContention {
		t.Errorf("err = %v", err)
	}
}

func TestFailingPrePushHookDoesNotAffectLockPushes(t *testing.T) {
	r := bare(t)
	a := client(t, r, "A")
	hook := filepath.Join(a.dir, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	acquire(t, a, "x-1")
}

func TestValidateID(t *testing.T) {
	for _, id := range []string{"bops-ss6.20.1", "a", "A_b-1"} {
		if err := ValidateID(id); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
	for _, id := range []string{"", ".a", "-a", "a/b", "a..b", "a.", "a.lock", "a b", "a\nb", "a:b", "a~1", "a^", "@{", strings.Repeat("a", 129)} {
		if CodeOf(ValidateID(id)) != CodeBadID {
			t.Errorf("%q should be refused", id)
		}
	}
	l := &Lock{Git: &fault{before: func(c Cmd) (Output, error, bool) {
		t.Errorf("git must not run: %v", c.Args)
		return Output{}, nil, true
	}}, Journal: newJournal()}
	if _, err := l.Acquire(ctx, "../../heads/main"); CodeOf(err) != CodeBadID {
		t.Errorf("err = %v", err)
	}
}

func TestEncodeRefusesLineBreaks(t *testing.T) {
	if _, err := (Message{Actor: "a\nrun=forged"}).encode(); CodeOf(err) != CodeBadMessage {
		t.Errorf("err = %v", err)
	}
}

func TestParseMessageTreatsInputAsUntrusted(t *testing.T) {
	m := ParseMessage("run=a=b\nrun=second\nextra=1\nnonsense\nstate=weird\nat=not-a-time\nactor=me")
	if m.Run != "a=b" || m.Actor != "me" || m.State != "" || !m.At.IsZero() {
		t.Errorf("m = %+v", m)
	}
	if m := ParseMessage("run=a\n" + strings.Repeat("x", maxMessage)); m.Run != "" {
		t.Errorf("oversized message should be ignored: %+v", m)
	}
}

func TestStaleness(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	window := 2 * time.Hour
	cases := []struct {
		name             string
		at               time.Time
		stale, anomalous bool
	}{
		{"fresh", now.Add(-time.Hour), false, false},
		{"at the window", now.Add(-window), false, false},
		{"old", now.Add(-3 * time.Hour), true, false},
		{"unknown", time.Time{}, false, false},
		{"slightly ahead", now.Add(time.Minute), false, false},
		{"far future", now.Add(time.Hour), false, true},
	}
	for _, c := range cases {
		m := Message{At: c.at}
		if m.Stale(now, window) != c.stale || m.Anomalous(now) != c.anomalous {
			t.Errorf("%s: stale=%v anomalous=%v", c.name, m.Stale(now, window), m.Anomalous(now))
		}
	}
}

func TestSSHCommand(t *testing.T) {
	cases := []struct {
		name, env, gitSSH, config, want string
	}{
		{"default", "", "", "", "ssh -o BatchMode=yes"},
		{"config kept", "", "", "ssh -i ~/.ssh/work", "ssh -i ~/.ssh/work -o BatchMode=yes"},
		{"env wins", "ssh -p 2222", "", "ssh -i x", "ssh -p 2222 -o BatchMode=yes"},
		{"full path", "/usr/bin/ssh", "", "", "/usr/bin/ssh -o BatchMode=yes"},
		{"already set", "ssh -o BatchMode=no", "", "", "ssh -o BatchMode=no"},
		{"wrapper left alone", "", "", "plink -batch", "plink -batch"},
		{"GIT_SSH left alone", "", "/opt/wrap", "", ""},
	}
	for _, c := range cases {
		getenv := func(k string) string {
			return map[string]string{"GIT_SSH_COMMAND": c.env, "GIT_SSH": c.gitSSH}[k]
		}
		if got := sshCommand(getenv, c.config); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParsePorcelain(t *testing.T) {
	const ref = "refs/bops/claims/x"
	cases := []struct {
		out  string
		want pushStatus
	}{
		{"To r\n*\tabc:" + ref + "\t[new reference]\nDone\n", pushed},
		{"To r\n+\tabc:" + ref + "\tabc...def (forced update)\nDone\n", pushed},
		{"To r\n-\t:" + ref + "\t[deleted]\nDone\n", pushed},
		{"To r\n!\tabc:" + ref + "\t[rejected] (stale info)\nDone\n", rejected},
		{"To r\n=\tabc:" + ref + "\t[up to date]\nDone\n", rejected},
		{"", noStatus},
		{"To r\n*\tabc:refs/bops/claims/other\t[new reference]\n", noStatus},
	}
	for _, c := range cases {
		if got := parsePorcelain(c.out, ref); got != c.want {
			t.Errorf("%q: got %v, want %v", c.out, got, c.want)
		}
	}
}

func TestSameRemote(t *testing.T) {
	same := [][2]string{
		{"https://github.com/o/r.git", "git+https://github.com/o/r.git"},
		{"git@github.com:o/r.git", "https://github.com/o/r"},
		{"ssh://git@github.com/o/r.git", "git+https://GitHub.com/o/r.git"},
		{"ssh://git@github.com:22/o/r", "git@github.com:o/r.git"},
		{"https://token@github.com/o/r.git", "https://github.com/o/r.git/"},
		{"/srv/git/r.git", "git+file:///srv/git/r.git"},
	}
	for _, c := range same {
		if !SameRemote(c[0], c[1]) {
			t.Errorf("%q and %q should match", c[0], c[1])
		}
	}
	differ := [][2]string{
		{"https://github.com/o/r.git", "https://github.com/o/other.git"},
		{"https://github.com/o/r.git", "https://gitlab.com/o/r.git"},
		{"/srv/git/r.git", "/srv/git/s.git"},
	}
	for _, c := range differ {
		if SameRemote(c[0], c[1]) {
			t.Errorf("%q and %q should differ", c[0], c[1])
		}
	}
}

func TestStoreSatisfiesJournal(t *testing.T) {
	var _ Journal = (*runstate.Store)(nil)
}
