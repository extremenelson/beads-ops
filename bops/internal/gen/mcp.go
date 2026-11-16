package gen

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// MCPTool is one tool in an MCP tools/list result.
type MCPTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

// MCPPageBytes is the most result text one tools/call returns; a longer
// result is paginated with a cursor (SPEC §8: under Claude Code's 25k-token
// result cap).
const MCPPageBytes = 20 * 1024

// MCPTools lists the commands with an mcp surface as MCP tools, in manifest
// order: kind=write commands only with enableWrites (SPEC §8). A tool's
// arguments are the command's: the bead id, text as a string (bops reads it
// on stdin), and every other argument by its own name. Each tool also takes
// cursor, to fetch the next page of a result over MCPPageBytes.
func MCPTools(m *manifest.Manifest, enableWrites bool) []MCPTool {
	var out []MCPTool
	for _, c := range m.Commands {
		mcp, ok := c.Surfaces["mcp"].(map[string]any)
		if !ok || (c.Kind == "write" && !enableWrites) {
			continue
		}
		readOnly, _ := mcp["readOnly"].(bool)
		props := map[string]any{}
		required := []string{}
		for _, an := range c.ArgNames() {
			a := c.Args[an]
			p := mcpProperty(an, a)
			props[an] = p
			switch k := cljArgKind(an, a); {
			case k == "id", k == "text", k == "value" && cljRequired(a):
				required = append(required, an)
			}
		}
		props["cursor"] = map[string]any{
			"type":        "string",
			"description": "Only to continue a paginated result: the cursor the previous page gave. Other arguments are then ignored.",
		}
		desc := c.Intent + ". Runs `bops " + c.Name + " --json` and returns its JSON document; exit 3 (a negative verdict, ok:false) is a result, not an error."
		if c.Kind == "write" {
			desc += " Writes: it changes the ledger or the claim refs on origin."
		}
		out = append(out, MCPTool{
			Name:        c.Name,
			Title:       "bops " + c.Name,
			Description: desc,
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           props,
				"required":             required,
				"additionalProperties": false,
			},
			Annotations: map[string]any{"readOnlyHint": readOnly},
		})
	}
	return out
}

// mcpProperty is the JSON Schema for one argument.
func mcpProperty(name string, a manifest.Arg) map[string]any {
	t := strings.TrimSuffix(a.Type, "?")
	p := map[string]any{}
	switch {
	case name == "id":
		p["type"], p["description"] = "string", "The bead id."
	case strings.HasPrefix(t, "stdin|file"):
		p["type"], p["description"] = "string", "The text, which bops reads on stdin."
	case t == "bool":
		p["type"] = "boolean"
	case t == "int":
		p["type"] = "integer"
	case t == "enum":
		p["type"], p["enum"] = "string", a.Values
	case strings.HasSuffix(t, "[]"):
		p["type"], p["items"] = "array", map[string]any{"type": "string"}
	case t == "url":
		p["type"], p["format"] = "string", "uri"
	case t == "duration":
		p["type"], p["description"] = "string", "A Go duration, such as 90m or 2h."
	default:
		p["type"] = "string"
	}
	if a.Doc != "" {
		p["description"] = a.Doc
	}
	if a.Default != nil {
		p["default"] = a.Default
	}
	return p
}

// MCP renders mcp/tools.json, the tools/list result `bops serve-mcp` serves,
// read-only or with enableWrites.
func MCP(m *manifest.Manifest, enableWrites bool) ([]File, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"tools": MCPTools(m, enableWrites)}); err != nil {
		return nil, err
	}
	return []File{{Path: "tools.json", Data: b.Bytes()}}, nil
}
