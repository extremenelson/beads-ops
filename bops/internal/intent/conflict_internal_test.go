package intent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bd 1.3.4's real stderr for the stopped pull is matched, and the branch it
// names is found; the error after a push or another pull failure is not.
func TestLedgerConflictMatchesBd134(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "bd-1.3.4-pull-operator-resolution.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	real := errors.New("bd dolt pull: exit status 1: " + strings.TrimSpace(string(b)))
	if !isLedgerConflict(real) {
		t.Errorf("not matched: %v", real)
	}
	if m := conflictRef.FindStringSubmatch(real.Error()); m == nil || m[1] != "origin/main" {
		t.Errorf("ref = %v", m)
	}
	for _, odd := range []string{
		"Error: merge -x: merge conflicts in issues require operator resolution",
		"Error: merge https://user:token@example.invalid/r.git: merge conflicts in issues require operator resolution",
	} {
		if m := conflictRef.FindStringSubmatch(odd); m != nil {
			t.Errorf("took %q from %q", m[1], odd)
		}
	}
	for _, other := range []string{
		"bd dolt push: exit status 1: Error: push to origin/main: Error 1105: ! [rejected] main -> main (non-fast-forward)",
		"bd dolt pull: exit status 1: Error: merge origin/main: resolve issues conflicts with 'theirs' strategy: resolve conflicts: Error 1105: resolving conflicts for table issues created foreign key violations",
		"bd dolt pull: exit status 1: Error 1105: no remote",
	} {
		if isLedgerConflict(errors.New(other)) {
			t.Errorf("matched %q", other)
		}
	}
}
