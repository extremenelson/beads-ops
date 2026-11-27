package gen

import (
	"encoding/json"
	"strings"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// Output types map mechanically: notes in parentheses are dropped, ? makes a
// field optional and nullable, [] a vector, a|b an enum.
func TestCljType(t *testing.T) {
	for in, want := range map[string]struct {
		s   string
		opt bool
	}{
		"string":             {":string", false},
		"int?":               {":int", true},
		"bool (a note here)": {":boolean", false},
		"string[]":           {"[:vector :string]", false},
		"string[]?":          {"[:vector :string]", true},
		"object?":            {":map", true},
		"ok|fail|skip":       {`[:enum "ok" "fail" "skip"]`, false},
		"string[] (any entry: exit 1, code from the first failure)": {"[:vector :string]", false},
	} {
		s, opt, err := cljType(in)
		if err != nil || s != want.s || opt != want.opt {
			t.Errorf("cljType(%q) = %q, %v, %v; want %q, %v", in, s, opt, err, want.s, want.opt)
		}
	}
	if _, _, err := cljType("float"); err == nil {
		t.Error("an unknown type must fail the generator, not pass silently")
	}
}

func TestCljSchemaNested(t *testing.T) {
	s, _, err := cljSchema(json.RawMessage(`[{"id":"string","why":"string[]","parent":"string?"}]`))
	if err != nil || s != "[:vector [:map [:id :string] [:parent {:optional true} [:maybe :string]] [:why [:vector :string]]]]" {
		t.Fatalf("%s, %v", s, err)
	}
}

func cljOut(t *testing.T) string {
	t.Helper()
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Clj(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "bops.clj" || files[1].Path != "bb.edn" {
		t.Fatalf("files = %v", files)
	}
	return string(files[0].Data)
}

// Each command's spec says how the command line takes its arguments, by the
// same rule as manifest.Arg.Flag and the docs.
func TestCljSpecs(t *testing.T) {
	out := cljOut(t)
	for _, want := range []string{
		`"claim" {:id? :optional :text nil :opts {:force :bool, :next :bool, :resume :bool} :required #{}}`,
		`"note" {:id? true :text :required :opts {} :required #{}}`,
		`"release" {:id? true :text :optional :opts {:reason :value} :required #{:reason}}`,
		`"land" {:id? true :text nil :opts {:forge :value, :ref :value, :sha :value, :verify :bool} :required #{:ref :sha}}`,
		`"next" {:id? false :text nil :opts {:caps :list, :include-unworkable :bool, :n :value, :pool :value} :required #{}}`,
		"(:refer-clojure :exclude [next sync])",
		"\n(defn note\n",
		"([cfg id text opts] (call cfg \"note\" id (assoc opts :text text)))",
		"\n(def contract\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bops.clj lacks %q", want)
		}
	}
}

// Every command whose surfaces.clj is "fn" gets a fn and an output schema.
func TestCljCoversEveryFnCommand(t *testing.T) {
	m, _ := manifest.Parse(bops.ManifestJSON)
	out := cljOut(t)
	for _, c := range m.Commands {
		if s, _ := c.Surfaces["clj"].(string); s != "fn" {
			continue
		}
		hasFn := strings.Contains(out, "\n(defn "+c.Name+"\n")
		schema := "\"" + c.Name + "\" [:map"
		hasSchema := strings.Contains(out, "\n   "+schema) || strings.Contains(out, "{"+schema)
		if !hasFn || !hasSchema {
			t.Errorf("bops %s: fn %v, output schema %v", c.Name, hasFn, hasSchema)
		}
	}
}
