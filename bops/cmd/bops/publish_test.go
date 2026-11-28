package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

func doctorWarnings(got map[string]any) []string {
	var out []string
	ws, _ := got["warnings"].([]any)
	for _, w := range ws {
		out = append(out, w.(string))
	}
	return out
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// A claim's failed ledger pull, recorded in the clone's state directory, is
// a doctor warning (the claim holds), and doctor leaves the record.
func TestDoctorWarnsOfARecordedClaimPullFailure(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	code, got := runDoctor(t, func(clone string) {
		s, err := runstate.Open(clone, runstate.Write)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.SetLedgerPullFailure(&runstate.LedgerPullFailure{At: time.Now(), Command: "claim", Bead: "x-1", Detail: "remote unreachable"}); err != nil {
			t.Fatal(err)
		}
	})
	if code != exitVerdict || got["ok"] != true {
		t.Fatalf("exit %d: %v", code, got)
	}
	if ws := doctorWarnings(got); !hasWarning(ws, "ledger not pulled: bops claim's ledger pull for x-1") || !hasWarning(ws, "remote unreachable") {
		t.Errorf("warnings = %v", ws)
	}
	if files, _ := filepath.Glob(filepath.Join(state, "bops", "*", "ledger-pull-failed.json")); len(files) != 1 {
		t.Errorf("doctor must leave the record: %v", files)
	}
}

// Doctor reports a local ledger ahead of its Dolt remote (claims not yet
// published), a diverged one, uncommitted rows, and counts from the last
// fetch when the remote did not answer, all as warnings.
func TestDoctorReportsAnUnpublishedLedger(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      []string
	}{
		{"published", `{"peers":[{"Reachable":true,"Status":{"LocalAhead":0,"LocalBehind":2,"Peer":"origin"}}],"pendingChanges":0}`, nil},
		{"ahead", `{"peers":[{"Reachable":true,"Status":{"LocalAhead":3,"LocalBehind":0,"Peer":"origin"}}],"pendingChanges":0}`,
			[]string{"ledger unpublished: 3 local commit(s) Dolt remote origin lacks, as of now", "bops sync"}},
		{"diverged", `{"peers":[{"Reachable":true,"Status":{"LocalAhead":2,"LocalBehind":1,"Peer":"origin"}}],"pendingChanges":0}`,
			[]string{"ledger diverged from Dolt remote origin, as of now: 2 local commit(s) it lacks and 1 of its not pulled"}},
		{"stale", `{"peers":[{"Reachable":false,"ReachError":"fetch from origin: Error 1105: fatal: no repo\nhint: x","Status":{"LocalAhead":1,"LocalBehind":0,"Peer":"origin"}}],"pendingChanges":0}`,
			[]string{"as of the last pull or fetch (the remote did not answer: fetch from origin: Error 1105: fatal: no repo)"}},
		{"unknown", `{"peers":[{"Reachable":false,"ReachError":"fetch from origin: no branch","Status":{"LocalAhead":-1,"LocalBehind":-1,"Peer":"origin"}}],"pendingChanges":0}`,
			[]string{"ledger publication unknown: bd has no tracking branch for Dolt remote origin", "the remote did not answer: fetch from origin: no branch"}},
		{"silent", `{"peers":[{"Reachable":false,"ReachError":"fetch from origin: timeout","Status":{"LocalAhead":0,"LocalBehind":0,"Peer":"origin"}}],"pendingChanges":0}`,
			[]string{"ledger publication not checked against Dolt remote origin: it did not answer (fetch from origin: timeout)"}},
		{"uncommitted", `{"peers":[{"Reachable":true,"Status":{"LocalAhead":0,"LocalBehind":0,"Peer":"origin"}}],"pendingChanges":4}`,
			[]string{"ledger has 4 uncommitted change(s)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := runDoctor(t, func(clone string) {
				origin, _ := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output()
				doc := strings.ReplaceAll(tc.doc, `"Reachable"`, `"URL":"git+file://`+strings.TrimSpace(string(origin))+`","Reachable"`)
				doctorBD.(*bdx.Fake).Responses["federation status --json"] = []byte(doc)
			})
			if code != exitVerdict || got["ok"] != true {
				t.Fatalf("exit %d: %v", code, got)
			}
			ws := doctorWarnings(got)
			for _, w := range tc.want {
				if !hasWarning(ws, w) {
					t.Errorf("warnings %v lack %q", ws, w)
				}
			}
			if tc.want == nil && hasWarning(ws, "ledger ") {
				t.Errorf("warnings = %v", ws)
			}
		})
	}
}

// A publication bd cannot report is a not-read warning; with the lock off
// doctor asks nothing (a claim still pushes there, and doctor touches no
// remote).
func TestDoctorPublicationUnreadAndLockOff(t *testing.T) {
	_, got := runDoctor(t, func(string) {
		delete(doctorBD.(*bdx.Fake).Responses, "federation status --json")
	})
	if ws := doctorWarnings(got); !hasWarning(ws, "ledger publication not read: ") {
		t.Errorf("warnings = %v", ws)
	}
	_, got = runDoctor(t, func(clone string) {
		if err := os.WriteFile(filepath.Join(clone, ".bops.json"), []byte(`{"lock":{"mode":"none"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	for _, c := range doctorBD.(*bdx.Fake).Calls {
		if len(c) > 0 && c[0] == "federation" {
			t.Errorf("doctor read the publication with the lock off: %v", c)
		}
	}
	if ws := doctorWarnings(got); hasWarning(ws, "ledger publication") {
		t.Errorf("warnings = %v", ws)
	}
}

// A Dolt remote bops does not push to (a mirror on another repository) is
// left out, so it cannot warn for ever; with none on the lock remote's
// repository the report says so. A failed remote check skips the fetch.
func TestDoctorPublicationOnlyCountsTheLockRemote(t *testing.T) {
	_, got := runDoctor(t, func(string) {
		doctorBD.(*bdx.Fake).Responses["federation status --json"] = []byte(`{"peers":[{"URL":"git+https://example.invalid/mirror.git","Reachable":true,"Status":{"LocalAhead":7,"LocalBehind":0,"Peer":"mirror"}}],"pendingChanges":0}`)
	})
	if ws := doctorWarnings(got); hasWarning(ws, "unpublished") || !hasWarning(ws, "bd lists no Dolt remote on the lock remote's repository") {
		t.Errorf("warnings = %v", ws)
	}
	_, got = runDoctor(t, func(clone string) { git(t, clone, "remote", "remove", "origin") })
	for _, c := range doctorBD.(*bdx.Fake).Calls {
		if len(c) > 0 && c[0] == "federation" {
			t.Errorf("doctor fetched for publication after its remote checks failed: %v", c)
		}
	}
	if ws := doctorWarnings(got); !hasWarning(ws, "ledger publication not read: the checks above did not confirm") {
		t.Errorf("warnings = %v", ws)
	}
}

// A shape this bops does not read is an error, not a report of zeros.
func TestDoctorPublicationRefusesAnUnknownShape(t *testing.T) {
	for _, doc := range []string{`{"peers":[],"pendingChanges":0,"schema_version":2}`, `{"peers":[{"URL":"%URL%","Reachable":true,"status":{"local_ahead":3}}]}`} {
		_, got := runDoctor(t, func(clone string) {
			origin, _ := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output()
			doctorBD.(*bdx.Fake).Responses["federation status --json"] = []byte(strings.ReplaceAll(doc, "%URL%", "git+file://"+strings.TrimSpace(string(origin))))
		})
		if ws := doctorWarnings(got); !hasWarning(ws, "ledger publication not read: bd federation status --json") {
			t.Errorf("%s: warnings = %v", doc, ws)
		}
	}
}
