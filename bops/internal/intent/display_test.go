package intent_test

import (
	"slices"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// A claim ref deleted with nobody taking the bead (a mistaken manual delete,
// say) leaves the ledger showing a claim nobody holds. The holder's sync
// gives the bead back to the pool; it never touches a bead someone else now
// holds, a closed bead, or a landing bead, which is sweep's to finish.
func TestSyncClearsTheDisplayOnlyWhenTheRefIsGoneAndTheBeadIsStillThisRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, a, b *rig, bd *fakeBD)
		cleared bool
	}{
		{"ref deleted, nobody took it", func(t *testing.T, a, b *rig, bd *fakeBD) {
			git(t, b.dir, "push", "-q", "--no-verify", "origin", ":refs/bops/claims/x-1")
		}, true},
		{"another run holds it", func(t *testing.T, a, b *rig, bd *fakeBD) {
			takeOver(t, b, "x-1")
		}, false},
		{"bead closed after the ref went", func(t *testing.T, a, b *rig, bd *fakeBD) {
			git(t, b.dir, "push", "-q", "--no-verify", "origin", ":refs/bops/claims/x-1")
			bd.beads["x-1"].Status = "closed"
		}, false},
		{"landing bead, sweep's crash case", func(t *testing.T, a, b *rig, bd *fakeBD) {
			git(t, b.dir, "push", "-q", "--no-verify", "origin", ":refs/bops/claims/x-1")
			bd.beads["x-1"].Labels = append(bd.beads["x-1"].Labels, "bops:landing")
		}, false},
		{"ledger now shows someone else's raw claim", func(t *testing.T, a, b *rig, bd *fakeBD) {
			git(t, b.dir, "push", "-q", "--no-verify", "origin", ":refs/bops/claims/x-1")
			n := bd.beads["x-1"]
			n.Assignee, n.Labels = "bob", []string{"run:BOB"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote, bd := bareRemote(t), newBD()
			bd.add("x-1")
			a := client(t, remote, "A", bd)
			b := client(t, remote, "B", bd)
			mustClaim(t, a, "x-1")
			tc.arrange(t, a, b, bd)
			status, assignee := bd.beads["x-1"].Status, bd.beads["x-1"].Assignee
			labels := slices.Clone(bd.beads["x-1"].Labels)

			out, err := intent.Sync(bg, a.env, false)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(out.LostClaims, []string{"x-1"}) || len(out.Warnings) != 0 {
				t.Fatalf("sync: %+v", out)
			}
			n := bd.beads["x-1"]
			if tc.cleared {
				if n.Status != "open" || n.Assignee != "" || slices.Contains(n.Labels, "run:A") {
					t.Errorf("display not cleared: %+v", n)
				}
				return
			}
			if n.Status != status || !slices.Equal(n.Labels, labels) || n.Assignee != assignee {
				t.Errorf("display changed: %+v, was status=%s assignee=%s labels=%v", n, status, assignee, labels)
			}
		})
	}
}

func TestClearedBeadIsClaimableAgain(t *testing.T) {
	remote, bd := bareRemote(t), newBD()
	bd.add("x-1")
	a := client(t, remote, "A", bd)
	b := client(t, remote, "B", bd)
	mustClaim(t, a, "x-1")
	git(t, b.dir, "push", "-q", "--no-verify", "origin", ":refs/bops/claims/x-1")
	if _, err := intent.Sync(bg, a.env, false); err != nil {
		t.Fatal(err)
	}
	mustClaim(t, b, "x-1")
}
