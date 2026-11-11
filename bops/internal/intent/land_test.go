package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
)

const (
	prURL = "https://github.com/o/r/pull/3"
	sha   = "abc1234"
)

// fakeForge answers from a map and counts calls.
type fakeForge struct {
	merged map[string]forge.Merged
	head   string
	err    error
	calls  int
}

func (f *fakeForge) Name() string { return "fake" }
func (f *fakeForge) IsMerged(_ context.Context, ref string) forge.Merged {
	f.calls++
	if m, ok := f.merged[ref]; ok {
		return m
	}
	return forge.Unknown
}
func (f *fakeForge) RefSHA(context.Context, string) (string, error) {
	f.calls++
	return f.head, f.err
}

// metaRunner reads the metadata file while bd would, since it is removed after.
type metaRunner struct {
	bdx.Fake
	metadata string
}

func (r *metaRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 4 && args[0] == "update" && args[2] == "--metadata" && strings.HasPrefix(args[3], "@") {
		r.Calls = append(r.Calls, args)
		b, err := os.ReadFile(strings.TrimPrefix(args[3], "@"))
		if err != nil {
			return nil, err
		}
		r.metadata = string(b)
		return []byte(`{}`), nil
	}
	return r.Fake.Run(ctx, args...)
}

func landRunner(show string) *metaRunner {
	return &metaRunner{Fake: bdx.Fake{Responses: map[string][]byte{
		"show x-1 --json": []byte(show),
		"comments add x-1 bops:landed run=RUN1 ref=" + prURL + " sha=" + sha: []byte(``),
		"dolt push": []byte(``),
	}}}
}

func heldWith(metadata string) string {
	return `[{"id":"x-1","status":"in_progress","labels":["run:RUN1"],"metadata":` + metadata + `,"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
}

func TestLandMergesMetadataAndKeepsForeignKeys(t *testing.T) {
	r := landRunner(heldWith(`{"foreign":{"keep":1},"bops":{"acceptance_checked_at":"t0"}}`))
	out, err := intent.Land(context.Background(), noteEnv(r), "x-1", intent.LandOpts{Ref: prURL, SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if out.Landed.Ref != prURL || out.Landed.Run != "RUN1" {
		t.Fatalf("%+v", out)
	}
	var got struct {
		Foreign map[string]int `json:"foreign"`
		Bops    struct {
			Contract int    `json:"contract"`
			Checked  string `json:"acceptance_checked_at"`
			Landed   []struct{ Ref, SHA, Run string }
		} `json:"bops"`
	}
	if err := json.Unmarshal([]byte(r.metadata), &got); err != nil {
		t.Fatalf("%v: %s", err, r.metadata)
	}
	if got.Foreign["keep"] != 1 || got.Bops.Checked != "t0" || got.Bops.Contract != 1 {
		t.Errorf("other keys changed: %s", r.metadata)
	}
	if len(got.Bops.Landed) != 1 || got.Bops.Landed[0].SHA != sha {
		t.Errorf("landed = %+v", got.Bops.Landed)
	}
	for _, c := range r.Calls {
		if c[0] == "close" {
			t.Errorf("land must never close: %v", c)
		}
	}
}

func TestLandVerify(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		ferr       error
		ok         bool
	}{
		{"matches a full sha", sha + "ffff", nil, true},
		{"different head", "ddd1234", nil, false},
		{"forge fails", "", errors.New("boom"), false},
	} {
		r := landRunner(heldWith(`{}`))
		f := &fakeForge{head: tc.head, err: tc.ferr}
		_, err := intent.Land(context.Background(), noteEnv(r), "x-1", intent.LandOpts{Ref: prURL, SHA: sha, Verify: f})
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if f.calls != 1 {
			t.Errorf("%s: forge calls = %d, want 1", tc.name, f.calls)
		}
		if !tc.ok && len(r.Calls) != 1 {
			t.Errorf("%s: must not write; calls = %v", tc.name, r.Calls)
		}
	}
}

func TestLandRefusesBadInputBeforeAnyCall(t *testing.T) {
	for _, o := range []intent.LandOpts{
		{Ref: "not a url", SHA: sha},
		{Ref: "ftp://x/y", SHA: sha},
		{Ref: prURL + " --evil", SHA: sha},
		{Ref: prURL, SHA: "XYZ"},
		{Ref: prURL, SHA: "abc"},
	} {
		r := landRunner(heldWith(`{}`))
		if _, err := intent.Land(context.Background(), newEnv(r), "x-1", o); !errors.Is(err, intent.ErrPrecondition) {
			t.Errorf("%+v: err = %v", o, err)
		}
		if len(r.Calls) != 0 {
			t.Errorf("%+v: calls = %v", o, r.Calls)
		}
	}
}

func TestLandRefusesWhenRunDoesNotHoldBead(t *testing.T) {
	r := landRunner(heldWith(`{}`))
	out, err := intent.Land(context.Background(), newEnv(r), "x-1", intent.LandOpts{Ref: prURL, SHA: sha})
	if err != nil || out.Outcome != intent.OutcomeRefused || !out.Negative() {
		t.Fatalf("%+v, %v", out, err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("must not write; calls = %v", r.Calls)
	}
}

func TestLandRepeatWritesNothing(t *testing.T) {
	r := landRunner(heldWith(`{"bops":{"landed":[{"ref":"` + prURL + `","sha":"` + sha + `","run":"RUN1","at":"t0"}]}}`))
	out, err := intent.Land(context.Background(), noteEnv(r), "x-1", intent.LandOpts{Ref: prURL, SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if out.Landed.At != "t0" {
		t.Errorf("should return the original record: %+v", out.Landed)
	}
	if len(r.Calls) != 1 {
		t.Errorf("repeat must not write; calls = %v", r.Calls)
	}
}

func TestLandRefusesMalformedMetadata(t *testing.T) {
	r := landRunner(heldWith(`{"bops":"not an object"}`))
	if _, err := intent.Land(context.Background(), noteEnv(r), "x-1", intent.LandOpts{Ref: prURL, SHA: sha}); err == nil {
		t.Fatal("want an error")
	}
	if len(r.Calls) != 1 {
		t.Errorf("must not write; calls = %v", r.Calls)
	}
}

func TestLiveLand(t *testing.T) {
	env, create := liveLedger(t)
	ctx := context.Background()
	id := create("land target")
	if _, err := env.BD.Run(ctx, "update", id, "--metadata", `{"foreign":{"keep":1}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Claim(ctx, env, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Land(ctx, env, id, intent.LandOpts{Ref: prURL, SHA: sha}); err != nil {
		t.Fatalf("land: %v", err)
	}
	got := liveShow(t, env, id)
	var meta struct {
		Foreign struct {
			Keep int `json:"keep"`
		} `json:"foreign"`
	}
	if err := json.Unmarshal(got.Metadata, &meta); err != nil || meta.Foreign.Keep != 1 {
		t.Errorf("foreign metadata lost: %v: %s", err, got.Metadata)
	}
	landed, err := got.Landed()
	if err != nil || len(landed) != 1 || landed[0].Ref != prURL {
		t.Fatalf("landed = %+v, %v", landed, err)
	}
}
