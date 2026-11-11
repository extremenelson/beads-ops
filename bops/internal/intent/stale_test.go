package intent_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// staleWorld builds one ledger seen from clone A, with claims on A, on a
// second machine B, and with no ref at all:
//
//	x-live     A, ref now                    not stale
//	x-here     A, ref 3h old                 stale, same machine
//	x-away     B (machine "far"), ref 3h     stale, other machine
//	x-land     A, landing ref 3h old         waiting on a PR: neither
//	x-future   A, ref dated 3h ahead         never stale
//	x-raw      no ref, updated 3h ago,       stale from the ledger; same
//	           claimed comment machine=box   machine through the comment
//	x-rawnote  no ref, old update, bops:     fresh: the comment counts
//	           note 10 minutes ago
//	x-hb       no ref, old update,           fresh: the heartbeat counts
//	           heartbeat 5 minutes ago
//	x-orphan   C, ref 3h old, ledger open    orphan
//	x-young    C, ref now, ledger open       not reported: probably pending
func staleWorld(t *testing.T) (a *rig, bd *fakeBD, now time.Time) {
	t.Helper()
	remote := bareRemote(t)
	bd = newBD()
	now = time.Now().UTC()
	old := now.Add(-3 * time.Hour)
	for _, id := range []string{"x-live", "x-here", "x-away", "x-land", "x-future", "x-orphan", "x-young"} {
		bd.add(id)
	}
	a = client(t, remote, "A", bd)
	b := client(t, remote, "B", bd)
	c := client(t, remote, "C", bd)
	w, err := runstate.Open(b.dir, runstate.Write) // B is another machine
	if err != nil {
		t.Fatal(err)
	}
	w.Run().Machine = "far"
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	w.Close() // B's store rereads it before each write
	at := func(r *rig, when time.Time) { r.env.Lock.Now = func() time.Time { return when } }

	mustClaim(t, a, "x-live")
	at(a, old)
	mustClaim(t, a, "x-here")
	mustClaim(t, a, "x-land")
	if out, err := intent.Release(bg, a.env, "x-land", "done"); err != nil || out.Outcome != intent.OutcomeReleased {
		t.Fatalf("release done: %+v, %v", out, err)
	}
	land := bd.beads["x-land"] // done keeps it in progress until sweep; display not yet cleared
	land.Status, land.Assignee, land.Labels = "in_progress", "tester", []string{"run:A", "bops:landing"}
	at(a, now.Add(3*time.Hour))
	mustClaim(t, a, "x-future")
	at(b, old)
	mustClaim(t, b, "x-away")
	at(c, old)
	mustClaim(t, c, "x-orphan")
	at(c, now)
	mustClaim(t, c, "x-young")
	for _, id := range []string{"x-orphan", "x-young"} {
		n := bd.beads[id]
		n.Status, n.Assignee, n.Labels = "open", "", nil // the ledger write never landed
	}
	for _, id := range []string{"x-live", "x-here", "x-away", "x-future"} {
		bd.beads[id].Updated = old // the ref, not updated_at, must decide
	}

	raw := bd.add("x-raw")
	raw.Status, raw.Assignee, raw.Labels, raw.Updated = "in_progress", "bob", []string{"run:RAW"}, old
	raw.Comments = []string{"bops:claimed run=RAW actor=bob machine=box"}
	raw.CommentTimes = []time.Time{old}
	note := bd.add("x-rawnote")
	note.Status, note.Assignee, note.Labels, note.Updated = "in_progress", "bob", []string{"run:RAW2"}, old
	note.Comments = []string{"bops:claimed run=RAW2 actor=bob machine=far", "bops:note run=RAW2\nNEXT: x"}
	note.CommentTimes = []time.Time{old, now.Add(-10 * time.Minute)}
	hb := bd.add("x-hb")
	hb.Status, hb.Assignee, hb.Updated = "in_progress", "carol", old
	beat := now.Add(-5 * time.Minute)
	hb.Heartbeat = &beat

	a.env.Now = func() time.Time { return now }
	return a, bd, now
}

func staleIDs(cs []intent.StaleClaim) []string {
	ids := []string{}
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestStaleListsIdleClaimsWithTheirMachine(t *testing.T) {
	a, bd, _ := staleWorld(t)
	bd.calls = nil

	out, err := intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: 2 * time.Hour, AllMachines: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := staleIDs(out.Stale), []string{"x-away", "x-here", "x-raw"}; !slices.Equal(got, want) {
		t.Fatalf("stale = %v, want %v (%+v)", got, want, out)
	}
	want := map[string]struct {
		same   bool
		source string
		run    string
	}{"x-away": {false, "ref", "B"}, "x-here": {true, "ref", "A"}, "x-raw": {true, "ledger", "run:RAW"}}
	for _, c := range out.Stale {
		w := want[c.ID]
		if c.SameMachine != w.same || c.Source != w.source || c.AgeMinutes < 179 || c.AgeMinutes > 181 {
			t.Errorf("%s: %+v, want same_machine=%v source=%s age 180", c.ID, c, w.same, w.source)
		}
		if strings.TrimPrefix(w.run, "run:") != strings.TrimPrefix(c.Run, "run:") {
			t.Errorf("%s: run %q, want %q", c.ID, c.Run, w.run)
		}
	}
	if len(out.Orphans) != 1 || out.Orphans[0].ID != "x-orphan" || out.Orphans[0].Run != "C" || out.Orphans[0].SameMachine {
		t.Errorf("orphans = %+v, want only x-orphan held by C on another machine", out.Orphans)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings: %v", out.Warnings)
	}
	for _, c := range bd.calls {
		if c[0] != "list" && !(c[0] == "comments" && c[1] != "add") {
			t.Errorf("stale must only read the ledger, ran bd %v", c)
		}
	}
}

func TestStaleThisMachineOnly(t *testing.T) {
	a, _, _ := staleWorld(t)
	out, err := intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := staleIDs(out.Stale), []string{"x-here", "x-raw"}; !slices.Equal(got, want) {
		t.Errorf("stale = %v, want %v", got, want)
	}
	if len(out.Orphans) != 0 {
		t.Errorf("another machine's orphan shown: %+v", out.Orphans)
	}
}

func TestStaleThreshold(t *testing.T) {
	a, _, _ := staleWorld(t)
	out, err := intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: 4 * time.Hour, AllMachines: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Stale) != 0 || len(out.Orphans) != 0 {
		t.Errorf("nothing is 4h old: %+v", out)
	}
}

func TestStaleOfflineUsesTheLastSyncAndSaysSo(t *testing.T) {
	a, _, _ := staleWorld(t)
	remote := strings.TrimSpace(git(t, a.dir, "remote", "get-url", "origin"))
	if err := os.Rename(remote, remote+".away"); err != nil {
		t.Fatal(err)
	}
	out, err := intent.Stale(bg, a.env, intent.StaleOpts{OlderThan: 2 * time.Hour, AllMachines: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "as of the last sync") {
		t.Errorf("warnings = %v", out.Warnings)
	}
	// A's own claims were recorded locally when it won them.
	if ids := staleIDs(out.Stale); !slices.Contains(ids, "x-here") {
		t.Errorf("stale = %v, want x-here from the local copy", ids)
	}
}
