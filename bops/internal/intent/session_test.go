package intent_test

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// hundredReady is a bd ready listing of n workable beads.
func hundredReady(n int) []byte {
	rows := []string{}
	for k := 0; k < n; k++ {
		rows = append(rows, fmt.Sprintf(`{"id":"r-%03d","title":"ready bead %d with an ordinary title","description":"d","acceptance_criteria":"ac","status":"open","priority":2,"issue_type":"task","owner":"o","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`, k, k))
	}
	return []byte("[" + strings.Join(rows, ",") + "]")
}

func TestSessionStartIsSmallOnAHundredBeads(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.ready = hundredReady(100)
	bd.prime = "# Beads Issue Tracker Active\n\nUse bd for task tracking."
	for _, id := range []string{"x-1", "x-2", "x-3"} {
		bd.add(id)
	}
	a := client(t, remote, "A", bd)
	for _, id := range []string{"x-1", "x-2", "x-3"} {
		mustClaim(t, a, id)
	}
	other := client(t, remote, "B", bd)
	other.env.Lock.Now = func() time.Time { return time.Now().Add(-3 * time.Hour) }
	bd.add("x-old")
	mustClaim(t, other, "x-old")
	bd.calls = nil

	out, err := intent.SessionStart(bg, a.env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Mine, []string{"x-1", "x-2", "x-3"}) || out.ReadyCount != 100 || out.Stale != 1 || !out.Synced {
		t.Errorf("got %+v", out)
	}
	if out.Prime != bd.prime {
		t.Errorf("prime = %q", out.Prime)
	}
	facts := *out
	facts.Prime = ""
	b, _ := json.Marshal(facts)
	const envelope = `{"contract":1,"command":"session-start","ok":true}`
	if n := len(b) + len(envelope); n >= 1024 {
		t.Errorf("facts are %d bytes, want under 1 kB: %s", n, b)
	}
	for _, c := range bd.calls {
		if c[0] == "dolt" && c[1] == "push" {
			t.Error("session-start must not push")
		}
		if c[0] == "prime" && !slices.Contains(c, "--mcp") {
			t.Errorf("prime must be the minimal form: %v", c)
		}
	}
}

func TestSessionStartStillReportsWhenSyncFails(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.prime = "rules"
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	mustClaim(t, a, "x-1")
	if err := os.Rename(remote, remote+".away"); err != nil {
		t.Fatal(err)
	}

	out, err := intent.SessionStart(bg, a.env)
	if err == nil || !strings.Contains(err.Error(), "sync failed") {
		t.Fatalf("err = %v", err)
	}
	if out == nil || out.Synced || !slices.Equal(out.Mine, []string{"x-1"}) || out.Prime != "rules" || out.ReadyCount != 2 {
		t.Errorf("a failed sync must still give the rest: %+v", out)
	}
}

func TestSessionStartCapsAHugePrime(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.prime = strings.Repeat("é", 4000) // a PRIME.md override can be any size
	a := client(t, remote, "A", bd)

	out, err := intent.SessionStart(bg, a.env)
	if err != nil {
		t.Fatal(err)
	}
	body, marker, found := strings.Cut(out.Prime, "\n… (cut at 3000 bytes")
	if !found || len(body) > 3000 || !strings.HasSuffix(marker, "run `bd prime` for the rest)") || !strings.HasPrefix(out.Prime, "é") {
		t.Errorf("prime not capped cleanly: %d bytes, ends %q", len(out.Prime), out.Prime[len(out.Prime)-60:])
	}
	if strings.ContainsRune(body, '�') || !strings.HasSuffix(body, "é") {
		t.Error("the cut split a character")
	}
}

func TestSessionStartWarnsAndContinuesWhenAPartFails(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.prime = "rules"
	a := client(t, remote, "A", bd)
	bd.fail = func(args []string) error {
		if args[0] == "ready" {
			return fmt.Errorf("ready broke")
		}
		return nil
	}

	out, err := intent.SessionStart(bg, a.env)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Warnings) != 1 || !strings.HasPrefix(out.Warnings[0], "next: ") || out.Prime != "rules" {
		t.Errorf("got %+v", out)
	}
}
