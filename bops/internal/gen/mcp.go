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
// order: kind=write commands only with enableWrites (SPEC §8), except a
// preview-only command (manifest agents "preview"), which is served as its
// dry run, read-only, with or without writes. A tool's arguments are the
// command's: the bead id, text as a string (bops reads it on stdin), and
// every other argument by its own name, except a person-only one (manifest
// arg agents "none"), which the tool leaves out. Each tool also takes
// cursor, to fetch the next page of a result over MCPPageBytes. A
// preview-only command keeps its dry-run argument, documented as true only;
// serve-mcp refuses false.
func MCPTools(m *manifest.Manifest, enableWrites bool) []MCPTool {
	var out []MCPTool
	for _, c := range m.Commands {
		mcp, ok := c.Surfaces["mcp"].(map[string]any)
		if !ok || (c.Kind == "write" && !enableWrites && !c.PreviewOnly()) {
			continue
		}
		readOnly, _ := mcp["readOnly"].(bool)
		props := map[string]any{}
		required := []string{}
		for _, an := range c.ArgNames() {
			a := c.Args[an]
			if a.PersonOnly() {
				continue // a person's, at the CLI: serve-mcp refuses it too
			}
			p := mcpProperty(an, a)
			if an == "dry-run" && c.PreviewOnly() {
				p["description"] = "Only true (or absent) here: false is refused. A real run is a person's, at the CLI."
			}
			props[an] = p
			switch k := cljArgKind(an, a); {
			case k == "id" && !strings.HasSuffix(a.Type, "?"), k == "text", k == "value" && cljRequired(a):
				required = append(required, an)
			}
		}
		props["cursor"] = map[string]any{
			"type":        "string",
			"description": "Only to continue a paginated result: the cursor the previous page gave. Other arguments are then ignored.",
		}
		desc := c.Intent + ". Runs `bops " + c.Name + " --json` and returns its JSON document; exit 3 (a negative verdict, ok:false) is a result, not an error."
		switch {
		case c.PreviewOnly():
			desc += " Preview only here: it changes neither the ledger nor the claim refs, and dry-run false is refused; hand a person the `" + RealRun(&c) + "` command to run."
		case c.Kind == "write":
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

// RealRun is the command a person runs at the CLI for a preview-only
// command's real run, built from the preview: reap's would_release entries
// each carry its --only value as only, <id>@<run> to pin the holder the
// entry showed (so a claim taken over since is skipped, not released), or
// the bare id when the run cannot be pinned; gc takes the ids it listed;
// sweep takes no --only and closes everything it would.
func RealRun(c *manifest.Command) string {
	run := "bops " + c.Name + " --dry-run=false"
	switch {
	case c.Name == "reap":
		return run + " --only <only>,... (each would_release entry's only: <id>@<run>, or the bare id when its run cannot be pinned; skip an entry without one)"
	case c.Args["only"].Type != "":
		return run + " --only <id>,... (from the preview)"
	}
	return run
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
