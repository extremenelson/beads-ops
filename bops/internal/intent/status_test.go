package intent_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

func statusByID(out *intent.StatusOut) map[string]intent.StatusClaim {
	m := map[string]intent.StatusClaim{}
	for _, c := range out.Claims {
		m[c.ID] = c
	}
	return m
}

func statusIDs(out *intent.StatusOut) []string {
	ids := []string{}
	for _, c := range out.Claims {
		ids = append(ids, c.ID)
	}
	return ids
}

func near(p *int, want int) bool { return p != nil && *p >= want-1 && *p <= want+1 }

// status shows every claim of staleWorld, joined from the refs and the
// ledger, in the state stale would give it, and writes nothing.
func TestStatusListsEveryClaim(t *testing.T) {
	a, bd, _ := staleWorld(t)
	pushBranch(t, a, "work/x-here")
	bd.beads["x-away"].Comments = append(bd.beads["x-away"].Comments, "bops:note run=B\nNEXT: y", "see https://github.com/o/r/pull/7")
	bd.beads["x-away"].CommentTimes = nil
	bd.calls = nil

	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"x-away", "x-future", "x-hb", "x-here", "x-land", "x-live", "x-orphan", "x-raw", "x-rawnote", "x-young"}
	if got := statusIDs(out); !slices.Equal(got, want) {
		t.Fatalf("claims = %v, want %v", got, want)
	}
	states := map[string]string{"x-away": "stale", "x-future": "unknown", "x-hb": "live", "x-here": "stale", "x-land": "landing",
		"x-live": "live", "x-orphan": "orphan", "x-raw": "stale", "x-rawnote": "live", "x-young": "live"}
	m := statusByID(out)
	for id, st := range states {
		if m[id].State != st {
			t.Errorf("%s: state %q, want %q (%+v)", id, m[id].State, st, m[id])
		}
	}
	if c := m["x-away"]; c.Machine != "far" || c.SameMachine || c.Run != "B" || c.Source != "ref+ledger" || !near(c.IdleMinutes, 180) || c.MR != "https://github.com/o/r/pull/7" {
		t.Errorf("x-away: %+v", c)
	}
	if c := m["x-here"]; c.Machine != "box" || !c.SameMachine || c.Branch != "work/x-here" || c.MR != "" {
		t.Errorf("x-here: %+v", c)
	}
	if c := m["x-land"]; c.MR != "https://github.com/o/r/pull/1" {
		t.Errorf("x-land: mr %q, want the landed ref", c.MR)
	}
	if c := m["x-raw"]; c.Source != "ledger" || c.Machine != "box" || !c.SameMachine || !near(c.AgeMinutes, 180) || !near(c.IdleMinutes, 180) {
		t.Errorf("x-raw: %+v", c)
	}
	if c := m["x-rawnote"]; !near(c.LastNoteMinutes, 10) || c.Machine != "far" || c.SameMachine || !near(c.AgeMinutes, 180) {
		t.Errorf("x-rawnote: %+v", c)
	}
	if c := m["x-land"]; c.LastEventAt == nil {
		t.Errorf("x-land: no last_event_at, want the landing ref's at")
	}
	if c := m["x-future"]; c.IdleMinutes != nil || c.LastEventAt != nil || c.State != intent.StatusUnknown {
		t.Errorf("x-future: %+v, want unknown with no idle time for a ref dated ahead", c)
	}
	if c := m["x-young"]; c.Source != "ref" || c.Run != "C" {
		t.Errorf("x-young: %+v", c)
	}
	if c := m["x-hb"]; c.AgeMinutes != nil || c.LastNoteMinutes != nil || !near(c.IdleMinutes, 5) {
		t.Errorf("x-hb: %+v", c)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings: %v", out.Warnings)
	}
	for _, c := range bd.calls {
		if c[0] != "list" && !(c[0] == "comments" && c[1] != "add") && strings.Join(c, " ") != "federation status --json" {
			t.Errorf("status must only read the ledger, ran bd %v", c)
		}
	}
}

func TestStatusFilters(t *testing.T) {
	a, _, _ := staleWorld(t)
	for _, tc := range []struct {
		o    intent.StatusOpts
		want []string
	}{
		{intent.StatusOpts{OlderThan: 2 * time.Hour, Stale: true}, []string{"x-away", "x-future", "x-here", "x-orphan", "x-raw"}},
		{intent.StatusOpts{OlderThan: 2 * time.Hour, Mine: true, Stale: true}, []string{"x-future", "x-here", "x-orphan", "x-raw"}}, // C runs on A's machine,
		{intent.StatusOpts{OlderThan: 2 * time.Hour, Mine: true}, []string{"x-future", "x-here", "x-land", "x-live", "x-orphan", "x-raw", "x-young"}},
		{intent.StatusOpts{OlderThan: 4 * time.Hour, Stale: true}, []string{"x-future"}},
	} {
		out, err := intent.Status(bg, a.env, tc.o)
		if err != nil {
			t.Fatal(err)
		}
		if got := statusIDs(out); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.o, got, tc.want)
		}
	}
}

// A failed bead list is a failed run; failed comment and remote reads only
// thin the board, with a warning.
func TestStatusFailures(t *testing.T) {
	a, bd, _ := staleWorld(t)
	bd.fail = func(args []string) error {
		if args[0] == "list" {
			return errors.New("bd down")
		}
		return nil
	}
	if _, err := intent.Status(bg, a.env, intent.StatusOpts{}); err == nil {
		t.Fatal("a failed bead list must fail status")
	}

	bd.fail = func(args []string) error {
		if args[0] == "comments" {
			return errors.New("no comments")
		}
		return nil
	}
	a.git.offline = true
	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Claims) != 10 {
		t.Errorf("claims = %v, want all 10 from the last sync's refs", statusIDs(out))
	}
	if c := statusByID(out)["x-raw"]; c.AgeMinutes != nil || c.Machine != "" {
		t.Errorf("x-raw without comments: %+v", c)
	}
	if len(out.Warnings) != 5 {
		t.Errorf("warnings = %q, want the fetch, the publication not read, branches, first comments and the rest", out.Warnings)
	}
}

// Under lock.mode none every claim comes from the ledger.
func TestStatusLockOff(t *testing.T) {
	a, _, _ := staleWorld(t)
	a.env.LockOff = true
	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Claims {
		if c.Source != "ledger" || c.Branch != "" {
			t.Errorf("%s: %+v, want ledger only", c.ID, c)
		}
	}
	if slices.Contains(statusIDs(out), "x-orphan") || len(out.Warnings) != 1 {
		t.Errorf("claims %v, warnings %v", statusIDs(out), out.Warnings)
	}
}

// pushRefMessage writes a claim ref with the given message straight to the
// remote, as a broken or hostile writer could.
func pushRefMessage(t *testing.T, r *rig, id, msg string) {
	t.Helper()
	tree := git(t, r.dir, "hash-object", "-w", "-t", "tree", "--stdin")
	commit := git(t, r.dir, "-c", "user.name=x", "-c", "user.email=x@example.invalid", "commit-tree", tree, "-m", msg)
	git(t, r.dir, "push", "-q", "-f", "origin", commit+":refs/bops/claims/"+id)
}

// What stale never calls stale, status does not either: a ref whose state is
// unreadable, and a ledger claim whose comments, which may hold a newer bops:
// event, could not be read. Both are unknown, and --stale leaves them out.
func TestStatusNeverGuessesStale(t *testing.T) {
	a, bd, _ := staleWorld(t)
	odd := bd.add("x-odd")
	odd.Status, odd.Assignee, odd.Labels, odd.Updated = "in_progress", "bob", []string{"run:OLD"}, time.Now().UTC().Add(-3*time.Hour)
	pushRefMessage(t, a, "x-odd", "run=OLD\nactor=bob\nstate=weird\nat=2020-01-01T00:00:00Z")

	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if c := statusByID(out)["x-odd"]; c.State != intent.StatusUnknown || c.Source != "ref+ledger" || c.IdleMinutes != nil {
		t.Errorf("unreadable ref: %+v, want unknown", c)
	}
	st, _ := intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: 2 * time.Hour, AllMachines: true})
	if slices.Contains(staleIDs(st.Stale), "x-odd") {
		t.Fatal("fixture: stale must not list x-odd either")
	}

	bd.fail = func(args []string) error {
		if args[0] == "comments" && (args[1] == "x-raw" || args[1] == "x-rawnote") {
			return errors.New("no comments")
		}
		return nil
	}
	out, err = intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	m := statusByID(out)
	for _, id := range []string{"x-raw", "x-rawnote"} { // x-rawnote is fresh only through its note
		if m[id].State != intent.StatusUnknown {
			t.Errorf("%s without comments: %q, want unknown", id, m[id].State)
		}
	}
	if m["x-here"].State != intent.StatusStale {
		t.Errorf("x-here: %q, a ref's age needs no comments", m["x-here"].State)
	}
	out, _ = intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour, Stale: true})
	if got := statusIDs(out); !slices.Equal(got, []string{"x-away", "x-future", "x-here", "x-odd", "x-orphan", "x-raw", "x-rawnote"}) {
		t.Errorf("--stale = %v, want the unknown rows kept: they may be stale", got)
	}
}

// A ref decides over the bops:landing label: a held stale ref on a labelled
// bead is stale or an orphan, as stale says. With no ref, the label is
// landing (lock.mode none).
func TestStatusRefBeatsTheLandingLabel(t *testing.T) {
	a, bd, _ := staleWorld(t)
	bd.beads["x-here"].Labels = append(bd.beads["x-here"].Labels, "bops:landing")
	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if c := statusByID(out)["x-here"]; c.State != intent.StatusStale {
		t.Errorf("held stale ref with the label: %q, want stale", c.State)
	}
}

// Under lock.mode none every state comes from the ledger.
func TestStatusLockOffStates(t *testing.T) {
	a, _, _ := staleWorld(t)
	a.env.LockOff = true
	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	m := statusByID(out)
	for id, want := range map[string]string{"x-land": "landing", "x-raw": "stale", "x-hb": "live", "x-rawnote": "live", "x-here": "stale"} {
		if m[id].State != want {
			t.Errorf("%s: %q, want %q", id, m[id].State, want)
		}
	}
}

// Age and machine come from the run's newest claimed comment; a bad landed
// record names its bead and the MR falls back to the comments.
func TestStatusNewestClaimAndBadLandedRecord(t *testing.T) {
	a, bd, now := staleWorld(t)
	raw := bd.beads["x-raw"]
	raw.Comments = []string{"bops:claimed run=RAW actor=bob machine=far", "bops:claimed run=RAW actor=bob machine=box", "PR https://github.com/o/r/pull/3"}
	raw.CommentTimes = []time.Time{now.Add(-5 * time.Hour), now.Add(-3 * time.Hour), now.Add(-3 * time.Hour)}
	raw.Metadata = []byte(`{"bops":{"landed":"nope"}}`)
	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c := statusByID(out)["x-raw"]
	if c.Machine != "box" || !near(c.AgeMinutes, 180) || c.MR != "https://github.com/o/r/pull/3" {
		t.Errorf("x-raw: %+v", c)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "landed metadata not read for x-raw") {
		t.Errorf("warnings = %q", out.Warnings)
	}
}

// --stale reads no comments for a claim its ref already shows fresh.
func TestStatusStaleSkipsCommentsOfFreshRefs(t *testing.T) {
	a, bd, _ := staleWorld(t)
	bd.calls = nil
	if _, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour, Stale: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range bd.calls {
		if c[0] == "comments" && slices.Contains([]string{"x-live", "x-young", "x-land", "x-hb"}, c[1]) {
			t.Errorf("read comments of fresh %s", c[1])
		}
	}
}

// A held ref on an in-progress bead with no ledger claim still shows the
// bead's title; a ref with an unreadable at is unknown.
func TestStatusRefRowKeepsTheTitleAndUnreadableAtIsUnknown(t *testing.T) {
	a, bd, _ := staleWorld(t)
	n := bd.add("x-bare")
	n.Status = "in_progress"
	pushRefMessage(t, a, "x-bare", "run=Z\nactor=z\nstate=held\nat=yesterday")
	out, err := intent.Status(bg, a.env, intent.StatusOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if c := statusByID(out)["x-bare"]; c.Source != "ref+ledger" || c.State != intent.StatusUnknown || c.Run != "Z" {
		t.Errorf("x-bare: %+v", c)
	}
}
