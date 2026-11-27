package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
	"github.com/extremenelson/beads-ops/bops/internal/mcp"
)

// mcpCallLimit bounds one tool call: the longest command budget (sweep's)
// with room to spare.
const mcpCallLimit = 6 * time.Minute

// cmdServeMCP serves the manifest's MCP tools on stdin and stdout until
// stdin closes: read-only tools unless --enable-writes (SPEC §8). Each call
// runs `bops <command> ... --json` with this same binary, in this directory,
// so a tool behaves exactly as the command does.
func cmdServeMCP(m *manifest.Manifest, rest []string, stdin io.Reader, stdout, stderr *os.File) int {
	writes := false
	for _, a := range rest {
		switch a {
		case "--enable-writes":
			writes = true
		default:
			fmt.Fprintln(stderr, "usage: bops serve-mcp [--enable-writes]")
			return exitUsage
		}
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "bops serve-mcp:", err)
		return exitCantRun
	}
	var tools []mcp.Tool
	for _, t := range gen.MCPTools(m, writes) {
		b, err := json.Marshal(t)
		if err != nil {
			fmt.Fprintln(stderr, "bops serve-mcp:", err)
			return exitCantRun
		}
		tools = append(tools, mcp.Tool{Name: t.Name, JSON: b})
	}
	s := &mcp.Server{
		Name: "bops", Version: version, Tools: tools, PageBytes: gen.MCPPageBytes, Keep: 32,
		Call: func(ctx context.Context, name string, args map[string]any) (string, bool) {
			c, _ := m.Lookup(name)
			argv, in, err := mcpArgv(c, args)
			if err != nil {
				return err.Error(), true
			}
			return runSelf(ctx, self, argv, in)
		},
	}
	if err := s.Serve(context.Background(), stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "bops serve-mcp:", err)
		return exitCantRun
	}
	return exitVerdict
}

// mcpArgv is the command line for a tool call: the bead id first and
// positional, every other argument as --name=value in name order, text on stdin (with
// --file - when the text is optional), then --json. An argument the command
// does not take, or a missing id, text or required value, is an error, as is
// dry-run other than true on a preview-only command, which always passes
// --dry-run=true.
func mcpArgv(c *manifest.Command, args map[string]any) ([]string, *string, error) {
	for k := range args {
		if _, ok := c.Args[k]; !ok {
			return nil, nil, fmt.Errorf("bops %s takes no argument %q", c.Name, k)
		}
	}
	if c.PreviewOnly() {
		// Agents preview only (manifest agents "preview"): a real run is a
		// person's decision, at the CLI (PROCESS §12).
		if v := args["dry-run"]; v != nil && v != true { // null is absent: a preview
			return nil, nil, fmt.Errorf("bops %s: dry-run must be true here: over MCP %s only previews, and a real run is a person's decision (PROCESS §12). Hand them the command: bops %s --dry-run=false --only %s", c.Name, c.Name, c.Name, gen.OnlyHint(c))
		}
		args = maps.Clone(args)
		if args == nil {
			args = map[string]any{}
		}
		args["dry-run"] = true // explicit, whatever the CLI's default
	}
	argv := []string{c.Name}
	var in *string
	var file bool
	// The id first, as people type it; then the rest in name order.
	names := c.ArgNames()
	if i := slices.Index(names, "id"); i > 0 {
		names = append([]string{"id"}, slices.Delete(names, i, i+1)...)
	}
	for _, an := range names {
		a := c.Args[an]
		v, given := args[an]
		if v == nil {
			given = false
			if _, present := args[an]; present && (c.Name == "reap" || c.Name == "gc") && an == "only" {
				// An explicit null is an empty list, which reap and gc refuse, never
				// "no filter": a preview that listed nothing must not widen.
				argv = append(argv, "--only=")
				continue
			}
		}
		text := strings.HasPrefix(a.Type, "stdin|file")
		optional := strings.HasSuffix(a.Type, "?") || a.Type == "bool" || a.Default != nil
		if !given {
			if !optional {
				return nil, nil, fmt.Errorf("bops %s needs %s", c.Name, an)
			}
			continue
		}
		switch {
		case an == "id":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, nil, fmt.Errorf("bops %s: id must be a non-empty string", c.Name)
			}
			if strings.HasPrefix(s, "-") { // the command line would read it as a flag, such as --next
				return nil, nil, fmt.Errorf("bops %s: id must not start with -", c.Name)
			}
			argv = append(argv, s)
		case text:
			s, ok := v.(string)
			if !ok {
				return nil, nil, fmt.Errorf("bops %s: %s must be a string", c.Name, an)
			}
			in, file = &s, strings.HasSuffix(a.Type, "?")
		default:
			s, err := mcpValue(a, v)
			if err != nil {
				return nil, nil, fmt.Errorf("bops %s: %s %v", c.Name, an, err)
			}
			argv = append(argv, "--"+an+"="+s)
		}
	}
	if file {
		argv = append(argv, "--file", "-")
	}
	return append(argv, "--json"), in, nil
}

// mcpValue renders one JSON argument value as its flag's value.
func mcpValue(a manifest.Arg, v any) (string, error) {
	switch t := strings.TrimSuffix(a.Type, "?"); {
	case t == "bool":
		if b, ok := v.(bool); ok {
			return strconv.FormatBool(b), nil
		}
		return "", errors.New("must be a boolean")
	case t == "int":
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10), nil
		}
		return "", errors.New("must be an integer")
	case strings.HasSuffix(t, "[]"):
		list, ok := v.([]any)
		if !ok {
			return "", errors.New("must be a list of strings")
		}
		out := make([]string, len(list))
		for i, e := range list {
			s, ok := e.(string)
			if !ok || strings.Contains(s, ",") {
				return "", errors.New("must be a list of strings without commas")
			}
			out[i] = s
		}
		return strings.Join(out, ","), nil
	default:
		s, ok := v.(string)
		if !ok {
			return "", errors.New("must be a string")
		}
		return s, nil
	}
}

// runSelf runs this binary with argv and returns its JSON document,
// compacted to spare the model's tokens. Exit 0 and 3 (a negative verdict)
// are results; anything else is an error, with the document if bops printed
// one and its stderr otherwise.
func runSelf(ctx context.Context, self string, argv []string, in *string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, mcpCallLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, argv...)
	if in != nil {
		cmd.Stdin = strings.NewReader(*in)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "bops " + strings.Join(argv, " ") + ": " + err.Error(), true
		}
		code = ee.ExitCode()
	}
	text := strings.TrimSpace(out.String())
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(text)) == nil {
		text = compact.String()
	}
	if text == "" {
		text = strings.TrimSpace(errb.String())
	}
	return text, code != exitVerdict && code != exitNegative
}
