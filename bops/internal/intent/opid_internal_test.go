package intent

import (
	"encoding/json"
	"testing"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
)

func TestBDCommentIDIsAStringOrANumber(t *testing.T) {
	for in, want := range map[string]string{`{"id":42}`: "42", `{"id":"c-1"}`: "c-1", `{"text":"t"}`: ""} {
		var c bdComment
		if err := json.Unmarshal([]byte(in), &c); err != nil || string(c.ID) != want {
			t.Errorf("%s: %q, %v", in, c.ID, err)
		}
	}
	var c bdComment
	if json.Unmarshal([]byte(`{"id":true}`), &c) == nil {
		t.Error("a bool id was accepted")
	}
	// bd's envelope form unwraps to the same list.
	var out []bdComment
	if err := json.Unmarshal(ledger.Unwrap([]byte(`{"data":[{"id":1,"text":"bops:note run=A op=x"}],"schema_version":1}`)), &out); err != nil || len(out) != 1 || out[0].ID != "1" {
		t.Errorf("envelope: %+v, %v", out, err)
	}
}

func TestEventFields(t *testing.T) {
	kind, f := eventFields("bops:released run=A reason=done op=o-1\nfree text op=x")
	if kind != "bops:released" || f["run"] != "A" || f["reason"] != "done" || f["op"] != "o-1" {
		t.Errorf("%s %v", kind, f)
	}
	if kind, _ := eventFields(""); kind != "" {
		t.Errorf("empty: %q", kind)
	}
}
