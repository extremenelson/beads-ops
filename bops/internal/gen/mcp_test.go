package gen_test

import (
	"slices"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// The tools are the commands with an mcp surface, in manifest order, minus
// kind=write unless writes are enabled; readOnlyHint is the surface's
// readOnly (SPEC §8).
func TestMCPToolsAreTheManifestMinusWrites(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, writes := range []bool{false, true} {
		var want []string
		for _, c := range m.Commands {
			if _, ok := c.Surfaces["mcp"].(map[string]any); ok && (writes || c.Kind != "write") {
				want = append(want, c.Name)
			}
		}
		var got []string
		for _, tool := range gen.MCPTools(m, writes) {
			got = append(got, tool.Name)
			c, _ := m.Lookup(tool.Name)
			ro := c.Surfaces["mcp"].(map[string]any)["readOnly"]
			if tool.Annotations["readOnlyHint"] != ro {
				t.Errorf("%s: readOnlyHint %v, surface readOnly %v", tool.Name, tool.Annotations["readOnlyHint"], ro)
			}
			props := tool.InputSchema["properties"].(map[string]any)
			for _, an := range c.ArgNames() {
				if props[an] == nil {
					t.Errorf("%s: no property for argument %s", tool.Name, an)
				}
			}
			if props["cursor"] == nil {
				t.Errorf("%s: no cursor", tool.Name)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("writes=%v: tools %v, want %v", writes, got, want)
		}
	}
	read := gen.MCPTools(m, false)
	if slices.ContainsFunc(read, func(t gen.MCPTool) bool { c, _ := m.Lookup(t.Name); return c.Kind == "write" }) {
		t.Error("a write command is a tool without --enable-writes")
	}
}

// Required arguments are the id, required text and values with no default.
func TestMCPRequired(t *testing.T) {
	m, _ := manifest.Parse(bops.ManifestJSON)
	req := map[string][]string{}
	for _, tool := range gen.MCPTools(m, true) {
		req[tool.Name] = tool.InputSchema["required"].([]string)
	}
	for name, want := range map[string][]string{
		"note": {"id", "text"}, "release": {"id", "reason"}, "next": {}, "deps": {"id"},
	} {
		if !slices.Equal(req[name], want) {
			t.Errorf("%s required = %v, want %v", name, req[name], want)
		}
	}
}
