// Package gen renders harness bindings from the manifest.
//
// Every generator is a pure function of the manifest (and, for prose, the
// templates embedded beside it) so bindings can be golden-tested (SPEC.md
// §9). Two targets are registered: docs (docs/COMMANDS.md) and cc (the Claude
// Code skill, hooks fragment and /bops-next command). The other bindings SPEC
// §8 plans (Codex, Hermes, Clojure, MCP) are not registered yet, and
// `bops gen <one of them>` exits 2 as an unknown target.
package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// File is one generated file, at a slash-separated path relative to the
// directory the binding is installed into.
type File struct {
	Path string
	Data []byte
}

// Generator renders one target as one or more files.
type Generator func(m *manifest.Manifest) ([]File, error)

var registry = map[string]Generator{
	"docs": func(m *manifest.Manifest) ([]File, error) {
		b, err := Docs(m)
		if err != nil {
			return nil, err
		}
		return []File{{Path: "COMMANDS.md", Data: b}}, nil
	},
	"cc": CC,
}

// Targets lists registered generator names.
func Targets() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lookup returns a generator by name.
func Lookup(name string) (Generator, bool) {
	g, ok := registry[name]
	return g, ok
}

// argCell renders how the command line takes an argument, by the rule in
// manifest.Arg.Flag: the id as a positional, text typed stdin|file through
// --file (optional text, stdin|file?, reads stdin only when asked with
// --file -; required text reads stdin when --file is absent), everything
// else as a flag of its own name.
func argCell(name string, a manifest.Arg) string {
	text := strings.HasPrefix(a.Type, "stdin|file")
	switch f := a.Flag(name); {
	case f == "":
		return "`<" + name + ">` (positional)"
	case text && strings.HasSuffix(a.Type, "?"):
		return "`--" + f + " <path>` \\| `--" + f + " -`"
	case text:
		return "`--" + f + " <path>`"
	default:
		return "`--" + f + "`"
	}
}

// Docs renders docs/COMMANDS.md.
func Docs(m *manifest.Manifest) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# bops commands (contract %d)\n\n", m.Contract)
	b.WriteString("Generated from `bops/commands.json` by `bops gen docs`. Do not edit by hand.\n\n")
	b.WriteString("Exit codes: ")
	keys := make([]string, 0, len(m.Common.Exit))
	for k := range m.Common.Exit {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "**%s** = %s", k, m.Common.Exit[k])
	}
	b.WriteString(".\n\n")

	b.WriteString("| Command | Intent | Kind |\n|---|---|---|\n")
	for _, c := range m.Commands {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", c.Name, c.Intent, c.Kind)
	}
	b.WriteString("\n")

	for _, c := range m.Commands {
		fmt.Fprintf(&b, "## `bops %s`\n\n", c.Name)
		if c.Intent != "" {
			fmt.Fprintf(&b, "%s\n\n", c.Intent)
		}
		if len(c.Args) > 0 {
			b.WriteString("| Arg | Type | Default | Doc |\n|---|---|---|---|\n")
			for _, an := range c.ArgNames() {
				a := c.Args[an]
				typ := strings.ReplaceAll(a.Type, "|", "\\|")
				if a.Type == "enum" {
					typ = "enum: " + strings.Join(a.Values, " \\| ")
				}
				def := ""
				if a.Default != nil {
					def = fmt.Sprintf("`%v`", a.Default)
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", argCell(an, a), typ, def, a.Doc)
			}
			b.WriteString("\n")
		}
		if len(c.Preconditions) > 0 {
			b.WriteString("Preconditions: " + strings.Join(c.Preconditions, "; ") + ".\n\n")
		}
		if len(c.Never) > 0 {
			b.WriteString("Never: " + strings.Join(c.Never, ", ") + ".\n\n")
		}
		if len(c.Effects) > 0 {
			ek := make([]string, 0, len(c.Effects))
			for k := range c.Effects {
				ek = append(ek, k)
			}
			sort.Strings(ek)
			b.WriteString("Effects by reason: ")
			for i, k := range ek {
				if i > 0 {
					b.WriteString("; ")
				}
				fmt.Fprintf(&b, "`%s` → %s", k, c.Effects[k])
			}
			b.WriteString(".\n\n")
		}
		if c.Retry != nil {
			fmt.Fprintf(&b, "Retry: on %s, %d time(s), after %d ms.\n\n", c.Retry.On, c.Retry.Times, c.Retry.AfterMS)
		}
		if len(c.Wraps) > 0 {
			b.WriteString("Wraps: `" + strings.Join(c.Wraps, "`, `") + "`.\n\n")
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, c.Output, "", "  "); err != nil {
			return nil, fmt.Errorf("gen docs: %s output: %w", c.Name, err)
		}
		b.WriteString("Output:\n\n```json\n")
		b.Write(pretty.Bytes())
		b.WriteString("\n```\n\n")
	}
	return b.Bytes(), nil
}
