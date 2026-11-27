package intent_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// A forced claim returns where the last attempt stopped: its holder, the
// NEXT/BLOCKER paragraph of the newest note, the branch and the MR.
func TestForceReturnsThePreviousAttempt(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	mustClaim(t, a, "x-1")
	tree := git(t, a.dir, "hash-object", "-w", "-t", "tree", "--stdin")
	commit := git(t, a.dir, "-c", "user.name=x", "-c", "user.email=x@example.invalid", "commit-tree", tree, "-m", "wip")
	git(t, a.dir, "push", "-q", "origin", commit+":refs/heads/work/x-1")
	git(t, a.dir, "push", "-q", "origin", commit+":refs/heads/x-10") // another bead's branch
	n := bdB.beads["x-1"]
	n.Assignee, n.Status, n.Labels = "old", "in_progress", []string{"run:A"}
	n.Comments = []string{
		"bops:note run=A\nfirst try\n\nNEXT: an older step",
		"see https://github.com/o/r/pull/7 for the draft",
		"bops:claimed run=A actor=old",
		"bops:note run=A\nwired the parser\n\nBLOCKER: the fixture is missing\n\nREF: docs/x.md",
	}
	later(b, 3*time.Hour)

	out := mustForce(t, b, "x-1")
	p := out.Previous
	if p == nil {
		t.Fatalf("no previous: %+v", out)
	}
	if p.Actor != "tester" || p.Run != "A" || p.AgeMinutes < 179 {
		t.Errorf("holder = %+v", p)
	}
	if p.LastNote != "BLOCKER: the fixture is missing" || p.NoteRun != "A" {
		t.Errorf("last_note = %q", p.LastNote)
	}
	if p.Branch != "work/x-1" {
		t.Errorf("branch = %q", p.Branch)
	}
	if p.MR != "https://github.com/o/r/pull/7" {
		t.Errorf("mr = %q", p.MR)
	}
}

// With no note, no branch and no MR the holder is still returned, and the
// rest is empty.
func TestForceWithNoPriorNote(t *testing.T) {
	_, b, _, bd := pair(t, "x-1")
	n := bd.beads["x-1"]
	n.Assignee, n.Status, n.Labels = "raw-user", "in_progress", []string{"run:OLD"}
	n.Comments = []string{"bops:claimed run=OLD actor=raw-user"}
	n.Updated = time.Now().UTC().Add(-3 * time.Hour)

	out := mustForce(t, b, "x-1")
	p := out.Previous
	if p == nil || p.Actor != "raw-user" || p.Run != "OLD" || p.AgeMinutes < 179 {
		t.Fatalf("previous = %+v", p)
	}
	if p.LastNote != "" || p.Branch != "" || p.MR != "" {
		t.Errorf("previous = %+v", p)
	}
}

// A landed ref is the MR, over a URL in a comment; a note with no NEXT or
// BLOCKER paragraph gives its last paragraph.
func TestForcePrefersTheLandedRef(t *testing.T) {
	_, b, _, bd := pair(t, "x-1")
	n := bd.beads["x-1"]
	n.Assignee, n.Status = "raw-user", "in_progress"
	n.Updated = time.Now().UTC().Add(-3 * time.Hour)
	markLanded(n)
	n.Comments = []string{"bops:note run=OLD\nfirst\n\nhalfway through", "https://github.com/o/r/pull/9"}

	p := mustForce(t, b, "x-1").Previous
	if p == nil || p.MR != "https://github.com/o/r/pull/1" || p.LastNote != "halfway through" {
		t.Fatalf("previous = %+v", p)
	}
}

// A comment read that fails is a warning; the takeover stands.
func TestForceKeepsTheClaimWhenCommentsCannotBeRead(t *testing.T) {
	_, b, _, bd := pair(t, "x-1")
	n := bd.beads["x-1"]
	n.Assignee, n.Status = "raw-user", "in_progress"
	n.Updated = time.Now().UTC().Add(-3 * time.Hour)
	bd.fail = func(args []string) error {
		if len(args) == 3 && args[0] == "comments" && args[2] == "--json" {
			return errors.New("bd down")
		}
		return nil
	}
	out := mustForce(t, b, "x-1")
	if out.Previous == nil || out.Previous.Actor != "raw-user" || out.Previous.LastNote != "" {
		t.Fatalf("previous = %+v", out.Previous)
	}
	if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "comments not read") }) {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// A branch listing that fails and landed metadata that cannot be read are
// warnings; the takeover stands and the rest is still read.
func TestForceKeepsTheClaimWhenBranchesOrLandedCannotBeRead(t *testing.T) {
	_, b, _, bd := pair(t, "x-1")
	n := bd.beads["x-1"]
	n.Assignee, n.Status = "raw-user", "in_progress"
	n.Updated = time.Now().UTC().Add(-3 * time.Hour)
	n.Metadata = json.RawMessage(`{"bops":{"landed":"not a list"}}`)
	n.Comments = []string{"bops:note run=OLD\nNEXT: carry on"}
	b.env.Lock.Git = &headsFail{Git: b.env.Lock.Git, n: 1}

	out := mustForce(t, b, "x-1")
	p := out.Previous
	if p == nil || p.LastNote != "NEXT: carry on" || p.Branch != "" || p.MR != "" {
		t.Fatalf("previous = %+v", p)
	}
	for _, want := range []string{"branches not read", "landed metadata not read"} {
		if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
			t.Errorf("no %q in %v", want, out.Warnings)
		}
	}
}

// The taker's own fresh branch, made before the takeover, is not the old
// holder's branch.
func TestForceSkipsTheTakersOwnBranch(t *testing.T) {
	_, b, _, bd := pair(t, "x-1")
	tree := git(t, b.dir, "hash-object", "-w", "-t", "tree", "--stdin")
	commit := git(t, b.dir, "-c", "user.name=x", "-c", "user.email=x@example.invalid", "commit-tree", tree, "-m", "base")
	git(t, b.dir, "branch", "work/x-1", commit)
	git(t, b.dir, "symbolic-ref", "HEAD", "refs/heads/work/x-1")
	n := bd.beads["x-1"]
	n.Assignee, n.Status = "raw-user", "in_progress"
	n.Updated = time.Now().UTC().Add(-3 * time.Hour)

	if p := mustForce(t, b, "x-1").Previous; p == nil || p.Branch != "" {
		t.Fatalf("previous = %+v", p)
	}
}

// A plain claim and a refused force carry no previous.
func TestOnlyATakeoverReturnsPrevious(t *testing.T) {
	a, b, _, bdB := pair(t, "x-1")
	if out := mustClaim(t, a, "x-1"); out.Previous != nil {
		t.Errorf("plain claim: %+v", out.Previous)
	}
	bdB.beads["x-1"].Assignee = "tester"
	out, err := intent.Claim(bg, b.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeRefused || out.Previous != nil {
		t.Fatalf("%+v, %v", out, err)
	}
}

// With the lock off the previous attempt comes from the ledger, and no
// branch is looked up.
func TestLockOffForceReturnsThePreviousAttempt(t *testing.T) {
	bd := newBD()
	n := bd.add("x-1")
	n.Assignee, n.Status, n.Labels = "other", "in_progress", []string{"run:B"}
	n.Updated = time.Now().Add(-3 * time.Hour)
	n.Comments = []string{"bops:note run=B\ndone the parser\n\nNEXT: write the tests"}
	r := lockOffRig(t, "A", bd)

	out, err := intent.Claim(bg, r.env, "x-1", true)
	if err != nil || out.Outcome != intent.OutcomeClaimed {
		t.Fatalf("%+v, %v", out, err)
	}
	p := out.Previous
	if p == nil || p.Actor != "other" || p.Run != "B" || p.LastNote != "NEXT: write the tests" || p.Branch != "" {
		t.Fatalf("previous = %+v", p)
	}
}
