package intent_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

func landedBead(id, labels, ref string) string {
	return `{"id":"` + id + `","status":"in_progress","labels":` + labels + `,"metadata":{"bops":{"landed":[{"ref":"` + ref + `","sha":"` + sha + `","run":"R","at":"t"}]}},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`
}

const (
	refMerged  = "https://github.com/o/r/pull/1"
	refOpen    = "https://github.com/o/r/pull/2"
	refUnknown = "https://github.com/o/r/pull/9"
)

func sweepFixture() (*bdx.Fake, *fakeForge) {
	list := "[" + strings.Join([]string{
		landedBead("x-merged", `["bops:landing"]`, refMerged),
		landedBead("x-open", `["bops:landing"]`, refOpen),
		landedBead("x-unknown", `["bops:landing"]`, refUnknown),
		landedBead("x-held", `["run:ACTIVE"]`, refMerged),
		`{"id":"x-plain","status":"in_progress","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`,
		`{"id":"x-bad","status":"in_progress","metadata":{"bops":"nope"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`,
	}, ",") + "]"
	return &bdx.Fake{Responses: map[string][]byte{
		"list --status in_progress --limit 0 --json":                                     []byte(list),
		"close x-merged --reason merged via https://github.com/o/r/pull/1 (" + sha + ")": []byte(``),
		"dolt push": []byte(``),
	}}, &fakeForge{merged: map[string]forge.Merged{
		refMerged: forge.Yes, refOpen: forge.No, refUnknown: forge.Unknown,
	}}
}

func TestSweepDryRunWritesNothing(t *testing.T) {
	b, f := sweepFixture()
	out, err := intent.Sweep(context.Background(), newEnv(b), f, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.WouldClose, ",") != "x-merged" || len(out.Closed) != 0 || !out.DryRun {
		t.Fatalf("%+v", out)
	}
	if len(b.Calls) != 1 || b.Calls[0][0] != "list" {
		t.Errorf("dry run must only read; calls = %v", b.Calls)
	}
}

func TestSweepClosesOnlyMergedAndSortsTheRest(t *testing.T) {
	b, f := sweepFixture()
	out, err := intent.Sweep(context.Background(), newEnv(b), f, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Closed, ",") != "x-merged" {
		t.Errorf("closed = %v", out.Closed)
	}
	if strings.Join(out.Pending, ",") != "x-open,x-held" {
		t.Errorf("pending = %v", out.Pending)
	}
	if strings.Join(out.Unknown, ",") != "x-unknown,x-bad" {
		t.Errorf("unknown = %v", out.Unknown)
	}
	if last := b.Calls[len(b.Calls)-1]; last[0] != "dolt" {
		t.Errorf("last call = %v", last)
	}
}

func TestSweepUsesTheLatestLanding(t *testing.T) {
	list := `[{"id":"x-1","status":"in_progress","metadata":{"bops":{"landed":[{"ref":"` + refMerged + `","sha":"` + sha + `"},{"ref":"` + refOpen + `","sha":"` + sha + `"}]}},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
	b := &bdx.Fake{Responses: map[string][]byte{"list --status in_progress --limit 0 --json": []byte(list)}}
	_, f := sweepFixture()
	out, err := intent.Sweep(context.Background(), newEnv(b), f, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Pending, []string{"x-1"}) || len(out.Closed) != 0 {
		t.Errorf("%+v", out)
	}
}

func TestSweepContinuesPastACloseFailure(t *testing.T) {
	list := "[" + landedBead("x-a", `[]`, refMerged) + "," + landedBead("x-b", `[]`, refMerged) + "]"
	b := &bdx.Fake{Responses: map[string][]byte{
		"list --status in_progress --limit 0 --json":                                []byte(list),
		"close x-b --reason merged via https://github.com/o/r/pull/1 (" + sha + ")": []byte(``),
		"dolt push": []byte(``),
	}}
	_, f := sweepFixture()
	out, err := intent.Sweep(context.Background(), newEnv(b), f, false)
	if err == nil {
		t.Error("the close failure must be reported")
	}
	if strings.Join(out.Closed, ",") != "x-b" || strings.Join(out.Unknown, ",") != "x-a" {
		t.Errorf("%+v", out)
	}
}

func TestSweepNothingToCloseDoesNotPush(t *testing.T) {
	b := &bdx.Fake{Responses: map[string][]byte{"list --status in_progress --limit 0 --json": []byte(`[]`)}}
	if _, err := intent.Sweep(context.Background(), newEnv(b), &fakeForge{}, false); err != nil {
		t.Fatal(err)
	}
	if len(b.Calls) != 1 {
		t.Errorf("calls = %v", b.Calls)
	}
}

func TestLiveSweep(t *testing.T) {
	env, create := liveLedger(t)
	ctx := context.Background()
	id := create("sweep target")
	if _, err := intent.Claim(ctx, env, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Land(ctx, env, id, intent.LandOpts{Ref: refMerged, SHA: sha}); err != nil {
		t.Fatalf("land: %v", err)
	}

	f := &fakeForge{merged: map[string]forge.Merged{refMerged: forge.Yes}}
	out, err := intent.Sweep(ctx, env, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(out.Pending, id) {
		t.Fatalf("a bead still held by its run must stay pending: %+v", out)
	}

	if _, err := intent.Release(ctx, env, id, "done"); err != nil {
		t.Fatal(err)
	}
	dry, err := intent.Sweep(ctx, env, f, true)
	if err != nil || !slices.Contains(dry.WouldClose, id) {
		t.Fatalf("dry run: %+v, %v", dry, err)
	}
	if got := liveShow(t, env, id); got.Status != "in_progress" {
		t.Errorf("dry run changed status to %s", got.Status)
	}
	out, err = intent.Sweep(ctx, env, f, false)
	if err != nil || !slices.Contains(out.Closed, id) {
		t.Fatalf("sweep: %+v, %v", out, err)
	}
	if got := liveShow(t, env, id); got.Status != "closed" {
		t.Errorf("status = %s, want closed", got.Status)
	}
}
