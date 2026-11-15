package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUnwrap(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"envelope array":   {`{"data":[{"id":"fx-1"}],"schema_version":1}`, `[{"id":"fx-1"}]`},
		"envelope object":  {` {"schema_version":1,"data":{"id":"c-1"}} `, `{"id":"c-1"}`},
		"envelope error":   {`{"data":{"error":"nope"},"schema_version":1}`, `{"error":"nope"}`},
		"raw array":        {`[{"id":"fx-1"}]`, `[{"id":"fx-1"}]`},
		"raw comment":      {`{"id":"c-1","issue_id":"fx-1","schema_version":1}`, `{"id":"c-1","issue_id":"fx-1","schema_version":1}`},
		"raw error":        {`{"error":"nope","hint":"h","schema_version":1}`, `{"error":"nope","hint":"h","schema_version":1}`},
		"data alone":       {`{"data":[]}`, `{"data":[]}`},
		"data with others": {`{"data":[],"schema_version":1,"more":true}`, `{"data":[],"schema_version":1,"more":true}`},
		"not json":         {"bd version 1.3.4\n", "bd version 1.3.4\n"},
		"empty":            {"", ""},
	} {
		if got := string(Unwrap([]byte(c.in))); got != c.want {
			t.Errorf("%s: Unwrap(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
}

// The fixtures are bd 1.3.4 output from a throwaway ledger, captured raw and
// with BD_JSON_ENVELOPE=1 (testdata/README.md). Both forms decode the same.
func TestDecodeIssuesRawAndEnvelopeAgree(t *testing.T) {
	for _, cmd := range []string{"ready", "show"} {
		raw, err := DecodeIssues(fixture(t, cmd+".raw.json"))
		if err != nil {
			t.Fatalf("%s raw: %v", cmd, err)
		}
		env, err := DecodeIssues(fixture(t, cmd+".envelope.json"))
		if err != nil {
			t.Fatalf("%s envelope: %v", cmd, err)
		}
		if len(raw) == 0 || !reflect.DeepEqual(raw, env) {
			t.Fatalf("%s: raw %+v\nenvelope %+v", cmd, raw, env)
		}
	}
}

func TestDecodeIssueRawAndEnvelopeAgree(t *testing.T) {
	raw, err := DecodeIssue(fixture(t, "show.raw.json"))
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeIssue(fixture(t, "show.envelope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw, env) {
		t.Fatalf("raw %+v\nenvelope %+v", raw, env)
	}
	if raw.ID != "fx-lsc" || !raw.HasLabel("area:parser") || len(raw.Dependencies) != 1 {
		t.Fatalf("decoded %+v", raw)
	}
}

// bd's error object, wrapped, is still an ErrBD.
func TestDecodeEnvelopeError(t *testing.T) {
	for name, decode := range map[string]func([]byte) error{
		"issues": func(b []byte) error { _, err := DecodeIssues(b); return err },
		"issue":  func(b []byte) error { _, err := DecodeIssue(b); return err },
	} {
		var e *ErrBD
		if err := decode(fixture(t, "error.envelope.json")); !errors.As(err, &e) || e.Msg != "no issues found matching the provided IDs" || e.Hint == "" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDecodeCommentIDRawAndEnvelope(t *testing.T) {
	for _, f := range []string{"comment.raw.json", "comment.envelope.json"} {
		if id, err := DecodeCommentID(fixture(t, f)); err != nil || id == "" {
			t.Errorf("%s: id %q, err %v", f, id, err)
		}
	}
}
