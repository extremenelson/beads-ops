package gen_test

import (
	"slices"
	"strings"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// The tools are the commands with an mcp surface, in manifest order, minus
// kind=write unless writes are enabled or the command is preview-only;
// readOnlyHint is the surface's readOnly (SPEC §8). A person-only argument
// is no property.
func TestMCPToolsAreTheManifestMinusWrites(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, writes := range []bool{false, true} {
		var want []string
		for _, c := range m.Commands {
			if _, ok := c.Surfaces["mcp"].(map[string]any); ok && (writes || c.Kind != "write" || c.PreviewOnly()) {
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
				if person := c.Args[an].PersonOnly(); (props[an] == nil) != person {
					t.Errorf("%s: property for argument %s is %v, person-only %v", tool.Name, an, props[an], person)
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
	if slices.ContainsFunc(read, func(t gen.MCPTool) bool { c, _ := m.Lookup(t.Name); return c.Kind == "write" && !c.PreviewOnly() }) {
		t.Error("a write command is a tool without --enable-writes")
	}
}

// reap, gc and sweep are served read-only, as their dry runs, without
// --enable-writes; claim takes no force on MCP (PROCESS §12).
func TestMCPPreviewsAreReadOnlyAndClaimTakesNoForce(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, writes := range []bool{false, true} {
		tools := map[string]gen.MCPTool{}
		for _, tool := range gen.MCPTools(m, writes) {
			tools[tool.Name] = tool
		}
		for _, name := range []string{"reap", "gc", "sweep"} {
			tool, ok := tools[name]
			if !ok {
				t.Errorf("writes=%v: no %s tool", writes, name)
				continue
			}
			if tool.Annotations["readOnlyHint"] != true || !strings.Contains(tool.Description, "Preview only here") {
				t.Errorf("writes=%v: %s is not a read-only preview: %v", writes, name, tool)
			}
		}
		if claim, ok := tools["claim"]; ok && claim.InputSchema["properties"].(map[string]any)["force"] != nil {
			t.Errorf("writes=%v: claim tool takes force", writes)
		}
		if _, ok := tools["claim"]; ok != writes {
			t.Errorf("writes=%v: claim tool served %v", writes, ok)
		}
	}
	sweep, _ := m.Lookup("sweep")
	if got := gen.RealRun(sweep); got != "bops sweep --dry-run=false" {
		t.Errorf("RealRun(sweep) = %q", got)
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
