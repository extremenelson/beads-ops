package intent_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// operatorResolution is the error bdx.Exec makes of bd 1.3.4's real stderr
// for a pull that stops for a person (testdata, captured from the
// ledger-contention harness's delete case).
func operatorResolution(t *testing.T) error {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "bd-1.3.4-pull-operator-resolution.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	return errors.New("bd dolt pull: exit status 1: " + strings.TrimSpace(string(b)))
}

// stuckAfter makes every dolt pull after the first n stop for a person, and
// every dolt push fail non-fast-forward once the ledger is stuck, as bd 1.3.4
// does once another machine's delete meets this clone's change of the row.
func stuckAfter(t *testing.T, bd *fakeBD, n int) {
	stop := operatorResolution(t)
	bd.fail = func(args []string) error {
		switch strings.Join(args, " ") {
		case "dolt pull":
			if n > 0 {
				n--
				return nil
			}
			return stop
		case "dolt push":
			return errors.New("bd dolt push: exit status 1: Error: push to origin/main: Error 1105: ! [rejected] main -> main (non-fast-forward)")
		}
		return nil
	}
}

func conflictEvents(t *testing.T) []runstate.Event {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "events.jsonl"))
	var out []runstate.Event
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var ev runstate.Event
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Step == "ledger_pull" {
				out = append(out, ev)
			}
		}
	}
	return out
}

func diffFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "bd-1.3.4-diff-head-origin.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A pull that stops for a person ends pushLedger's retries at once, with its
// own code, the beads and the runbook, and is recorded for status and doctor.
func TestPushLedgerStopsOnALedgerConflict(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.calls = nil
	bd.diff = diffFixture(t)
	stuckAfter(t, bd, 1) // sync's own pull succeeds; pushLedger's retry pull stops
	a.env.Command = "sync"

	_, err := intent.Sync(bg, a.env, false)
	if lock.CodeOf(err) != lock.CodeLedgerConflict {
		t.Fatalf("err = %v (code %q)", err, lock.CodeOf(err))
	}
	for _, want := range []string{"ct-xmw", "MULTI-DEV-PLAN.md §6", "--strategy theirs", "without its labels and comments", "require operator resolution"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if p, q := bd.count("dolt pull"), bd.count("dolt push"); p != 2 || q != 1 {
		t.Errorf("pulls %d, pushes %d: want 2 and 1 (no retry after the conflict); calls %v", p, q, bd.calls)
	}
	if bd.count("diff HEAD origin/main --json") != 1 {
		t.Errorf("the beads must be read from bd diff against the branch bd named: %v", bd.calls)
	}
	rec, rerr := a.store.LedgerConflict()
	if rerr != nil || rec == nil || !rec.BeadsRead || !slices.Equal(rec.Beads, []string{"ct-xmw"}) || rec.Command != "sync" || rec.Ref != "origin/main" || rec.Run != "A" {
		t.Fatalf("record = %+v, %v", rec, rerr)
	}
	evs := conflictEvents(t)
	if len(evs) != 1 || evs[0].Outcome != "conflict" || evs[0].Code != lock.CodeLedgerConflict || !strings.Contains(evs[0].Detail, "ct-xmw") || evs[0].Command != "sync" {
		t.Errorf("events = %+v", evs)
	}

	// status reports it from the record, and pulls nothing.
	bd.calls = nil
	st, err := intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if st.LedgerConflict == nil || !slices.Equal(st.LedgerConflict.Beads, []string{"ct-xmw"}) || !strings.Contains(st.LedgerConflict.Next, "MULTI-DEV-PLAN.md §6") {
		t.Fatalf("status ledger_conflict = %+v", st.LedgerConflict)
	}
	if !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.HasPrefix(w, "ledger stuck: ") && strings.Contains(w, "ct-xmw") }) {
		t.Errorf("warnings = %v", st.Warnings)
	}
	if bd.count("dolt") != 0 {
		t.Errorf("status must not pull or push: %v", bd.calls)
	}

	// Once a person has resolved it, the next command whose pull succeeds
	// clears it.
	bd.fail = nil
	a = rigAt(t, a.dir, "A", bd)
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if rec, _ := a.store.LedgerConflict(); rec != nil {
		t.Errorf("record kept after a pull succeeded: %+v", rec)
	}
	if evs := conflictEvents(t); len(evs) != 2 || evs[1].Outcome != "resolved" {
		t.Errorf("events = %+v", evs)
	}
	st, _ = intent.Status(bg, a.env, intent.StatusOpts{})
	if st.LedgerConflict != nil {
		t.Errorf("status still reports %+v", st.LedgerConflict)
	}
}

// Once one pull in a command has stopped, later pulls and pushes in that
// command return the same error without calling bd.
func TestALedgerConflictIsNotRetriedWithinTheCommand(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	bd.calls = nil
	stuckAfter(t, bd, 0)

	_, err := intent.Sync(bg, a.env, false)
	if lock.CodeOf(err) != lock.CodeLedgerConflict {
		t.Fatalf("err = %v", err)
	}
	_, err = intent.Sync(bg, a.env, false) // the same Env: the same command
	if lock.CodeOf(err) != lock.CodeLedgerConflict {
		t.Fatalf("second err = %v", err)
	}
	if p, q := bd.count("dolt pull"), bd.count("dolt push"); p != 1 || q != 0 {
		t.Errorf("pulls %d, pushes %d: want 1 and 0; calls %v", p, q, bd.calls)
	}
	// bd diff failed (the fake has none), so the beads are not known, and
	// the report says so rather than naming none.
	rec, _ := a.store.LedgerConflict()
	if rec == nil || rec.BeadsRead || len(rec.Beads) != 0 {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.Contains(err.Error(), "which beads conflict could not be read") {
		t.Errorf("err = %v", err)
	}
}

// A claim --force whose pull stops reports ledger_conflict, not unreachable.
func TestClaimForceReportsALedgerConflict(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	stuckAfter(t, bd, 0)
	out, err := intent.Claim(bg, a.env, "x-1", true)
	if lock.CodeOf(err) != lock.CodeLedgerConflict || out.Outcome != intent.OutcomeError {
		t.Fatalf("out %+v, err %v", out, err)
	}
}

// Any other pull failure keeps its old handling: no record, and retried.
func TestAnOrdinaryPullFailureIsNotAConflict(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	bd.fail = func(args []string) error {
		if strings.Join(args, " ") == "dolt pull" {
			return errors.New("bd dolt pull: exit status 1: Error: fetch: connection refused")
		}
		return nil
	}
	_, err := intent.Sync(bg, a.env, false)
	if err == nil || lock.CodeOf(err) == lock.CodeLedgerConflict {
		t.Fatalf("err = %v", err)
	}
	if rec, _ := a.store.LedgerConflict(); rec != nil {
		t.Errorf("record = %+v", rec)
	}
}

// A bead deleted here and changed there is named too: it is on the remote
// and not here.
func TestALedgerConflictNamesBeadsOnlyTheRemoteHas(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd)
	bd.diff = []byte(`[{"IssueID":"x-7","DiffType":"added"},{"IssueID":"x-8","DiffType":"modified"}]`)
	stuckAfter(t, bd, 0)
	_, err := intent.Sync(bg, a.env, false)
	if lock.CodeOf(err) != lock.CodeLedgerConflict || !strings.Contains(err.Error(), "origin/main has that this clone does not (deleted here, or created there): x-7") {
		t.Fatalf("err = %v", err)
	}
	rec, _ := a.store.LedgerConflict()
	if rec == nil || len(rec.Beads) != 0 || !slices.Equal(rec.RemoteOnly, []string{"x-7"}) {
		t.Fatalf("record = %+v", rec)
	}
}

// A pull that succeeds clears only a record older than its own start: a
// newer one is another process's conflict met after this pull began.
func TestASucceedingPullKeepsANewerRecord(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd)
	newer := &runstate.LedgerConflict{At: time.Now().Add(time.Hour), Detail: "d", Beads: []string{}}
	if err := a.store.SetLedgerConflict(newer); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if rec, _ := a.store.LedgerConflict(); rec == nil {
		t.Fatal("a record newer than the pull was cleared")
	}
}

// An unreadable record is a warning in status, and a pull that succeeds
// removes it.
func TestAnUnreadableRecordWarnsAndIsClearedByAPull(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd)
	files, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "bops", "*", "state.json"))
	if len(files) != 1 {
		t.Fatalf("state files %v", files)
	}
	path := filepath.Join(filepath.Dir(files[0]), "ledger-conflict.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := intent.Status(bg, a.env, intent.StatusOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if st.LedgerConflict != nil || !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.HasPrefix(w, "ledger conflict record not read: ") }) {
		t.Fatalf("status = %+v", st)
	}
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unreadable record kept after a pull succeeded: %v", err)
	}
}

// A record that cannot be written still leaves the command its code and
// runbook, and says the record is missing.
func TestAConflictThatCannotBeRecordedSaysSo(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	a := client(t, remote, "A", bd)
	a.env.Store = unwritableConflict{a.store}
	stuckAfter(t, bd, 0)
	_, err := intent.Sync(bg, a.env, false)
	if lock.CodeOf(err) != lock.CodeLedgerConflict || !strings.Contains(err.Error(), "not recorded for status and doctor: disk full") || !strings.Contains(err.Error(), "MULTI-DEV-PLAN.md §6") {
		t.Fatalf("err = %v", err)
	}
	if evs := conflictEvents(t); len(evs) != 1 || !strings.Contains(evs[0].Detail, "not recorded") {
		t.Errorf("events = %+v", evs)
	}
}

type unwritableConflict struct{ *runstate.Store }

func (unwritableConflict) SetLedgerConflict(*runstate.LedgerConflict) error {
	return errors.New("disk full")
}

// A release whose push meets a stuck ledger warns with the conflict's code
// and runbook, not a bare push failure.
func TestReleaseOnAStuckLedgerSaysAPersonMustResolveIt(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	stuckAfter(t, bd, 0)
	out, err := intent.Release(bg, a.env, "x-1", "scope")
	if err != nil || !out.LedgerPending {
		t.Fatalf("%+v, %v", out, err)
	}
	if !slices.ContainsFunc(out.Warnings, func(w string) bool {
		return strings.Contains(w, lock.CodeLedgerConflict) && strings.Contains(w, "MULTI-DEV-PLAN.md §6")
	}) {
		t.Errorf("warnings = %v", out.Warnings)
	}
}
