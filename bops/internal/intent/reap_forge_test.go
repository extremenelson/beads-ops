package intent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

// searchForge is a fake forge that answers SearchMRs from a map of bead id to
// merge requests, or with err, and logs what it was asked.
type searchForge struct {
	mrs   map[string][]forge.MR
	err   error
	block bool // wait for the context instead of answering
	asked []string
	// later, when set, answers every search after the first.
	later map[string][]forge.MR
	// laterErr, when set, fails every search after the first.
	laterErr error
}

func (f *searchForge) Name() string                                   { return "fake" }
func (f *searchForge) IsMerged(context.Context, string) forge.Merged  { return forge.Unknown }
func (f *searchForge) RefSHA(context.Context, string) (string, error) { return "", errors.New("no") }
func (f *searchForge) SearchMRs(ctx context.Context, text string) ([]forge.MR, error) {
	f.asked = append(f.asked, text)
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.laterErr != nil && len(f.asked) > 1 {
		return nil, f.laterErr
	}
	if f.later != nil && len(f.asked) > 1 {
		return f.later[text], f.err
	}
	return f.mrs[text], f.err
}

const forkPR = "https://github.com/someone/fork/pull/9"

func TestReapKeepsAClaimWithAnOpenOrMergedMROnTheForge(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-open", "x-merged", "x-closed", "x-other", "x-idle", "x-odd")
	f := &searchForge{mrs: map[string][]forge.MR{
		// Opened from a fork: no branch on any remote this clone sees.
		"x-open": {{URL: forkPR, Title: "Some work", Branch: "patch/x-open", State: "open"}},
		// Merged with its branch deleted.
		"x-merged": {{URL: "https://github.com/o/r/pull/3", Title: "fix the thing (x-merged)", Branch: "gone", State: "merged"}},
		// A state the code does not know keeps the claim.
		"x-odd": {{URL: "https://github.com/o/r/pull/6", Title: "x-odd", State: "draft"}},
		// Closed without merging is not work.
		"x-closed": {{URL: "https://github.com/o/r/pull/4", Title: "x-closed attempt", Branch: "work/x-closed", State: "closed"}},
		// The search found it only in the body: neither title nor branch names it.
		"x-other": {{URL: "https://github.com/o/r/pull/5", Title: "unrelated x-other2", Branch: "main", State: "open"}},
	}}

	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	kept := keptEvidence(out)
	if kept["x-open"] != "mr "+forkPR+" (open, found on the forge)" {
		t.Errorf("x-open evidence = %q", kept["x-open"])
	}
	if kept["x-merged"] != "mr https://github.com/o/r/pull/3 (merged, found on the forge)" {
		t.Errorf("x-merged evidence = %q", kept["x-merged"])
	}
	if kept["x-odd"] != "mr https://github.com/o/r/pull/6 (draft, found on the forge)" {
		t.Errorf("x-odd evidence = %q", kept["x-odd"])
	}
	if got := reapIDs(out.WouldRelease); got != "x-closed,x-idle,x-other" {
		t.Errorf("would_release = %q, want x-closed,x-idle,x-other", got)
	}
	if slices.Contains(out.Warnings, intent.ReapNoForge) {
		t.Errorf("warned of no forge with one: %v", out.Warnings)
	}
	for _, id := range []string{"x-open", "x-merged"} {
		if bdB.wrote(id) {
			t.Errorf("dry run wrote %s", id)
		}
	}
}

func TestReapKeepsTheClaimWhenTheForgeDoesNotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *searchForge
		want string
		warn bool // the run warns the forge did not answer
	}{
		{"error", &searchForge{err: errors.New("gh: HTTP 502")}, "forge unanswered, so kept: gh: HTTP 502", true},
		{"incomplete", &searchForge{err: forge.ErrIncomplete, mrs: map[string][]forge.MR{
			"x-1": {{URL: "https://github.com/o/r/pull/4", Title: "nothing", Branch: "main", State: "open"}}}},
			"forge unanswered, so kept: " + forge.ErrIncomplete.Error(), false},
		// The command has no deadline: only the forge's own clock stops it.
		{"timeout", &searchForge{block: true}, "forge unanswered, so kept: context deadline exceeded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, b, bdB := reapWorld(t, "x-1")
			start := time.Now()
			out, err := intent.Reap(bg, b.env, intent.ReapOpts{Forge: tc.f, ForgeClock: 200 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 10*time.Second {
				t.Errorf("forge search was not bounded: %s", time.Since(start))
			}
			if len(out.Released) != 0 || keptEvidence(out)["x-1"] != tc.want {
				t.Fatalf("want x-1 kept with %q: %+v", tc.want, out)
			}
			if bdB.wrote("x-1") || bdB.beads["x-1"].Assignee != "tester" {
				t.Errorf("claim was touched: %+v", bdB.beads["x-1"])
			}
			if slices.Contains(out.Warnings, intent.ReapForgeUnanswered) != tc.warn {
				t.Errorf("warnings = %v, want the unanswered forge named: %v", out.Warnings, tc.warn)
			}
		})
	}
}

func TestReapReleasesWhenTheForgeFindsNothingAndSaysSo(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-idle", "x-branch")
	pushBranch(t, b, "work/x-branch")
	f := &searchForge{}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Released) != "x-idle" {
		t.Fatalf("%+v", out)
	}
	// A search per claim with no other evidence, before and after its
	// takeover; none for the one a branch keeps.
	if !slices.Equal(f.asked, []string{"x-idle", "x-idle"}) {
		t.Errorf("forge asked about %v, want only x-idle", f.asked)
	}
	if all := strings.Join(bdB.beads["x-idle"].Comments, "\n"); !strings.Contains(all, "nor did the forge find an open or merged merge request") {
		t.Errorf("comments = %v", bdB.beads["x-idle"].Comments)
	}
}

func TestReapWarnsWithoutAForgeToSearch(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1")
	for _, f := range []forge.Forge{nil, mustNone(t)} {
		out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Forge: f})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(out.Warnings, intent.ReapNoForge) || reapIDs(out.WouldRelease) != "x-1" {
			t.Errorf("forge %v: %+v", f, out)
		}
	}
}

func mustNone(t *testing.T) forge.Forge {
	t.Helper()
	f, err := forge.Select(bg, "none", "", func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("no remote")
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReapForgeTitleMatching(t *testing.T) {
	for _, tc := range []struct {
		title, id string
		want      bool
	}{
		{"fix: reap asks the forge (bops-260)", "bops-260", true},
		{"BOPS-260: reap", "bops-260", true},
		{"bops-2601 something", "bops-260", false},
		{"issue 260 fixed", "bops-260", false}, // a bare suffix is not enough in prose
		{"epic work (bops-260)", "bops-260.2", true},
		{"abops-260", "bops-260", false},
	} {
		f := &searchForge{mrs: map[string][]forge.MR{tc.id: {{URL: "https://github.com/o/r/pull/1", Title: tc.title, State: "open"}}}}
		_, b, _ := reapWorld(t, tc.id)
		out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Forge: f})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(out.Kept) == 1; got != tc.want {
			t.Errorf("title %q, id %q: kept = %v, want %v", tc.title, tc.id, got, tc.want)
		}
	}
}

// A merge request opened between the first search and the takeover keeps the
// claim: the forge is asked again under the bead lock.
func TestReapSearchesTheForgeAgainAfterTheTakeover(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	f := &searchForge{later: map[string][]forge.MR{"x-1": {{URL: forkPR, Title: "x-1: work", State: "open"}}}}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Released) != 0 || len(out.Failed) != 1 || out.Failed[0].Outcome != intent.OutcomeHeld ||
		!strings.Contains(out.Failed[0].Detail, "taken over, then found work: mr "+forkPR) {
		t.Fatalf("want the claim held after the takeover: %+v", out)
	}
	if len(f.asked) != 2 {
		t.Errorf("forge asked %d times, want 2", len(f.asked))
	}
	if bdB.beads["x-1"].Status != "in_progress" {
		t.Errorf("ledger = %+v", bdB.beads["x-1"])
	}
}

// A preview with too little time left for a forge search lists the rest
// under skipped rather than keep them on searches the deadline cut short.
func TestReapPreviewStopsSearchingWhenTimeRunsOut(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1", "x-2")
	f := &searchForge{}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	out, err := intent.Reap(ctx, b.env, intent.ReapOpts{DryRun: true, Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if reapIDs(out.Skipped) != "x-1,x-2" || len(f.asked) != 0 || len(out.WouldRelease) != 0 {
		t.Fatalf("want both skipped unsearched: asked %v, %+v", f.asked, out)
	}
}

// A full page that does contain a match is evidence, not an unanswered forge.
func TestReapKeepsAMatchFromAFullPage(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1")
	f := &searchForge{err: forge.ErrIncomplete, mrs: map[string][]forge.MR{
		"x-1": {{URL: forkPR, Title: "x-1: work", State: "open"}}}}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if keptEvidence(out)["x-1"] != "mr "+forkPR+" (open, found on the forge)" || slices.Contains(out.Warnings, intent.ReapForgeUnanswered) {
		t.Fatalf("%+v", out)
	}
}

// A search that fails after the takeover keeps the claim with this run.
func TestReapHoldsWhenTheForgeFailsAfterTheTakeover(t *testing.T) {
	_, b, bdB := reapWorld(t, "x-1")
	f := &searchForge{laterErr: errors.New("gh: HTTP 502")}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Released) != 0 || len(out.Failed) != 1 || out.Failed[0].Outcome != intent.OutcomeHeld ||
		!strings.Contains(out.Failed[0].Detail, "forge unanswered, so kept") || !slices.Contains(out.Warnings, intent.ReapForgeUnanswered) {
		t.Fatalf("%+v", out)
	}
	if bdB.beads["x-1"].Status != "in_progress" {
		t.Errorf("ledger = %+v", bdB.beads["x-1"])
	}
}

// After one search goes unanswered, the rest are not made: a dead forge
// costs one clock, not one per claim.
func TestReapAsksADeadForgeOnce(t *testing.T) {
	_, b, _ := reapWorld(t, "x-1", "x-2")
	f := &searchForge{err: errors.New("exec: gh: not found")}
	out, err := intent.Reap(bg, b.env, intent.ReapOpts{DryRun: true, Forge: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.asked) != 1 || len(out.Kept) != 2 || !strings.Contains(keptEvidence(out)["x-2"], "an earlier search failed: exec: gh: not found") {
		t.Fatalf("asked %v: %+v", f.asked, out)
	}
}

// With no forge to wait on, a preview's deadline skips nothing, and a claim
// local evidence keeps is listed as kept, not skipped.
func TestReapPreviewTimeGateOnlyGuardsSearches(t *testing.T) {
	a, b, _ := reapWorld(t, "x-1", "x-2")
	pushBranch(t, a, "work/x-1")
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	out, err := intent.Reap(ctx, b.env, intent.ReapOpts{DryRun: true})
	if err != nil || reapIDs(out.WouldRelease) != "x-2" || len(out.Skipped) != 0 {
		t.Fatalf("no forge: err = %v, %+v", err, out)
	}
	out, err = intent.Reap(ctx, b.env, intent.ReapOpts{DryRun: true, Forge: &searchForge{}})
	if err != nil || len(out.Kept) != 1 || out.Kept[0].ID != "x-1" || reapIDs(out.Skipped) != "x-2" {
		t.Fatalf("forge: err = %v, %+v", err, out)
	}
}
