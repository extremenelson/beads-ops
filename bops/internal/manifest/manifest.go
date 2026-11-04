// Package manifest loads and validates commands.json.
package manifest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Manifest is the parsed commands.json.
type Manifest struct {
	Contract int       `json:"contract"`
	Common   Common    `json:"common"`
	Commands []Command `json:"commands"`
}

// Common holds flags and semantics shared by every command.
type Common struct {
	Flags    map[string]Arg    `json:"flags"`
	Exit     map[string]string `json:"exit"`
	Envelope map[string]string `json:"envelope"`
}

// Command is one intent-level command.
type Command struct {
	Name          string            `json:"name"`
	Intent        string            `json:"intent"`
	Kind          string            `json:"kind"` // read | write | read+sync
	Args          map[string]Arg    `json:"args"`
	Preconditions []string          `json:"preconditions,omitempty"`
	Effects       map[string]string `json:"effects,omitempty"`
	Never         []string          `json:"never,omitempty"`
	Lint          []string          `json:"lint,omitempty"`
	Wraps         []string          `json:"wraps"`
	Retry         *Retry            `json:"retry,omitempty"`
	Output        json.RawMessage   `json:"output"`
	Surfaces      map[string]any    `json:"surfaces"`
}

// Arg describes one argument or flag.
type Arg struct {
	Type    string   `json:"type"`
	Default any      `json:"default,omitempty"`
	Values  []string `json:"values,omitempty"`
	Doc     string   `json:"doc,omitempty"`
}

// Retry describes a bounded retry policy.
type Retry struct {
	On      string `json:"on"`
	Times   int    `json:"times"`
	AfterMS int    `json:"after_ms"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

var validKinds = map[string]bool{"read": true, "write": true, "read+sync": true}

// Parse decodes and validates a manifest.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate enforces the structural rules from SPEC.md §4.
func (m *Manifest) Validate() error {
	if m.Contract < 1 {
		return fmt.Errorf("manifest: contract must be >= 1, got %d", m.Contract)
	}
	if len(m.Commands) == 0 {
		return fmt.Errorf("manifest: no commands")
	}
	seen := map[string]bool{}
	for i, c := range m.Commands {
		where := fmt.Sprintf("manifest: commands[%d] (%q)", i, c.Name)
		if !nameRE.MatchString(c.Name) {
			return fmt.Errorf("%s: bad name", where)
		}
		if seen[c.Name] {
			return fmt.Errorf("%s: duplicate name", where)
		}
		seen[c.Name] = true
		if c.Intent == "" && c.Name != "version" {
			return fmt.Errorf("%s: missing intent", where)
		}
		if !validKinds[c.Kind] {
			return fmt.Errorf("%s: bad kind %q", where, c.Kind)
		}
		if len(c.Output) == 0 {
			return fmt.Errorf("%s: missing output", where)
		}
		if len(c.Surfaces) == 0 {
			return fmt.Errorf("%s: missing surfaces", where)
		}
		for an, a := range c.Args {
			if a.Type == "" {
				return fmt.Errorf("%s: arg %q has no type", where, an)
			}
			if a.Type == "enum" && len(a.Values) == 0 {
				return fmt.Errorf("%s: enum arg %q has no values", where, an)
			}
		}
		if c.Kind == "write" {
			if mcp, ok := c.Surfaces["mcp"].(map[string]any); ok {
				if ro, _ := mcp["readOnly"].(bool); ro {
					return fmt.Errorf("%s: write command cannot be mcp readOnly", where)
				}
			}
		}
	}
	return nil
}

// Lookup returns the command by name.
func (m *Manifest) Lookup(name string) (*Command, bool) {
	for i := range m.Commands {
		if m.Commands[i].Name == name {
			return &m.Commands[i], true
		}
	}
	return nil, false
}

// Names returns command names in manifest order.
func (m *Manifest) Names() []string {
	out := make([]string, 0, len(m.Commands))
	for _, c := range m.Commands {
		out = append(out, c.Name)
	}
	return out
}

// ArgNames returns a command's argument names, sorted, for deterministic output.
func (c *Command) ArgNames() []string {
	out := make([]string, 0, len(c.Args))
	for k := range c.Args {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
