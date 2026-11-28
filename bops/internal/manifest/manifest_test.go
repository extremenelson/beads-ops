package manifest_test

import (
	"strings"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

func TestEmbeddedManifestIsValid(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if m.Contract != 1 {
		t.Fatalf("contract = %d, want 1 (bump this test deliberately when the contract changes)", m.Contract)
	}
	for _, want := range []string{"next", "claim", "note", "release", "land", "sweep", "stale", "lint", "sync", "session-start", "gates", "deps", "version"} {
		if _, ok := m.Lookup(want); !ok {
			t.Errorf("missing command %q", want)
		}
	}
}

// reap and gc take over or delete other runs' claims and sweep closes beads,
// so agents only preview them, and claim's force is a person's (PROCESS §12).
func TestAgentLimits(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m.Commands {
		want := c.Name == "reap" || c.Name == "gc" || c.Name == "sweep"
		if c.PreviewOnly() != want {
			t.Errorf("%s: PreviewOnly = %v, want %v", c.Name, c.PreviewOnly(), want)
		}
		for an, a := range c.Args {
			if want := c.Name == "claim" && an == "force"; a.PersonOnly() != want {
				t.Errorf("%s %s: PersonOnly = %v, want %v", c.Name, an, a.PersonOnly(), want)
			}
		}
	}
}

func TestArgFlag(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		want string
	}{
		{"id", "string", ""},
		{"text", "stdin|file", "file"},
		{"text", "stdin|file?", "file"},
		{"reason", "enum", "reason"},
		{"dry-run", "bool", "dry-run"},
	}
	for _, c := range cases {
		if got := (manifest.Arg{Type: c.typ}).Flag(c.name); got != c.want {
			t.Errorf("Arg{%q}.Flag(%q) = %q, want %q", c.typ, c.name, got, c.want)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"dup":                     `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"read","output":{},"surfaces":{"cc":[]}},{"name":"a","intent":"x","kind":"read","output":{},"surfaces":{"cc":[]}}]}`,
		"bad kind":                `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"rw","output":{},"surfaces":{"cc":[]}}]}`,
		"bad name":                `{"contract":1,"commands":[{"name":"Bad_Name","intent":"x","kind":"read","output":{},"surfaces":{"cc":[]}}]}`,
		"no output":               `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"read","surfaces":{"cc":[]}}]}`,
		"enum empty":              `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"read","args":{"r":{"type":"enum"}},"output":{},"surfaces":{"cc":[]}}]}`,
		"write ro":                `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","output":{},"surfaces":{"mcp":{"readOnly":true}}}]}`,
		"bad agents":              `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","agents":"none","args":{"dry-run":{"type":"bool","default":true}},"output":{},"surfaces":{"cc":[]}}]}`,
		"preview no dry-run":      `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","agents":"preview","output":{},"surfaces":{"cc":[]}}]}`,
		"preview dry-run default": `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","agents":"preview","args":{"dry-run":{"type":"bool","default":false}},"output":{},"surfaces":{"cc":[]}}]}`,
		"preview not ro":          `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","agents":"preview","args":{"dry-run":{"type":"bool","default":true}},"output":{},"surfaces":{"mcp":{"readOnly":false}}}]}`,
		"bad arg agents":          `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","args":{"f":{"type":"bool","default":false,"agents":"preview"}},"output":{},"surfaces":{"cc":[]}}]}`,
		"none no default":         `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","args":{"f":{"type":"bool","agents":"none"}},"output":{},"surfaces":{"cc":[]}}]}`,
		"none not bool":           `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","args":{"f":{"type":"string","agents":"none"}},"output":{},"surfaces":{"cc":[]}}]}`,
		"none id":                 `{"contract":1,"commands":[{"name":"a","intent":"x","kind":"write","args":{"id":{"type":"bool","default":false,"agents":"none"}},"output":{},"surfaces":{"cc":[]}}]}`,
		"contract 0":              `{"contract":0,"commands":[{"name":"a","intent":"x","kind":"read","output":{},"surfaces":{"cc":[]}}]}`,
	}
	for name, src := range cases {
		if _, err := manifest.Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: unexpected %v", name, err)
		}
	}
}
