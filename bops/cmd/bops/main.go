// Command bops is the intent-level command layer for beads. See SPEC.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// version is set by -ldflags "-X main.version=..."; "dev" otherwise.
var version = "dev"

const (
	exitVerdict = 0
	exitCantRun = 1
	exitUsage   = 2
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		fmt.Fprintln(stderr, "bops: embedded manifest invalid:", err)
		return exitCantRun
	}
	if len(args) == 0 {
		usage(stderr, m)
		return exitUsage
	}
	name, rest := args[0], args[1:]
	wantJSON := !isTerminal(stdout)
	for _, a := range rest {
		if a == "--json" {
			wantJSON = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch name {
	case "version":
		return cmdVersion(ctx, m, wantJSON, stdout, stderr)
	case "manifest":
		stdout.Write(bops.ManifestJSON)
		return exitVerdict
	case "gen":
		return cmdGen(m, rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout, m)
		return exitVerdict
	}
	if _, ok := m.Lookup(name); ok {
		fmt.Fprintf(stderr, "bops %s: specified in contract %d but not yet implemented (did NOT run)\n", name, m.Contract)
		return exitCantRun
	}
	fmt.Fprintf(stderr, "bops: unknown command %q\n", name)
	usage(stderr, m)
	return exitUsage
}

// isTerminal reports whether f is a character device (stdlib-only; SPEC §11a).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func usage(w *os.File, m *manifest.Manifest) {
	fmt.Fprintf(w, "usage: bops <command> [flags]   (contract %d)\n\ncommands:\n", m.Contract)
	for _, c := range m.Commands {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Intent)
	}
	fmt.Fprintf(w, "  %-14s %s\n  %-14s %s\n", "manifest", "print the embedded commands.json", "gen <target>", "render a binding ("+strings.Join(gen.Targets(), ", ")+")")
}

type versionOut struct {
	Contract int    `json:"contract"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	Bops     string `json:"bops"`
	Bd       string `json:"bd"`
	Backend  string `json:"backend"`
	Warning  string `json:"warning,omitempty"`
}

func cmdVersion(ctx context.Context, m *manifest.Manifest, asJSON bool, stdout, stderr *os.File) int {
	out := versionOut{Contract: m.Contract, Command: "version", OK: true, Bops: version}
	v, err := bdx.DetectVersion(ctx, bdx.Exec{})
	switch {
	case errors.Is(err, bdx.ErrNotFound):
		out.Bd, out.Warning = "", "bd not found on PATH"
	case err != nil:
		out.Bd, out.Warning = "", err.Error()
	default:
		out.Bd, out.Backend = v.Version, v.Backend
		if !strings.HasPrefix(strings.TrimPrefix(v.Version, "v"), "1.") {
			out.Warning = "bd " + v.Version + " is outside the tested range (>= 1.3)"
		}
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Fprintf(stdout, "bops=%s\ncontract=%d\nbd=%s\nbackend=%s\n", out.Bops, out.Contract, out.Bd, out.Backend)
		if out.Warning != "" {
			fmt.Fprintln(stderr, "warning:", out.Warning)
		}
	}
	return exitVerdict
}

func cmdGen(m *manifest.Manifest, rest []string, stdout, stderr *os.File) int {
	if len(rest) == 0 || rest[0] == "--list" {
		fmt.Fprintln(stdout, strings.Join(gen.Targets(), "\n"))
		return exitVerdict
	}
	g, ok := gen.Lookup(rest[0])
	if !ok {
		fmt.Fprintf(stderr, "bops gen: unknown target %q (have: %s)\n", rest[0], strings.Join(gen.Targets(), ", "))
		return exitUsage
	}
	b, err := g(m)
	if err != nil {
		fmt.Fprintln(stderr, "bops gen:", err)
		return exitCantRun
	}
	stdout.Write(b)
	return exitVerdict
}
