// Command bops is the intent-level command layer for beads. See SPEC.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
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
	// exitNegative means bops ran and the answer was no: the bead is someone
	// else's or a precondition failed. `bops claim x && work` must not proceed.
	exitNegative = 3
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
	ctx, cancel := context.WithTimeout(context.Background(), budget(name))
	defer cancel()
	gateWarnings = nil
	if name == "guard" { // a hook on every Bash call: no gate, no network, fails open
		return cmdGuard(m, rest, wantJSON, os.Stdin, isTerminal(os.Stdin), stdout, stderr)
	}
	if c, ok := m.Lookup(name); ok && name != "doctor" {
		if warn, err := minBopsGate(c.Kind); err != nil {
			if name == "session-start" && slices.Contains(rest, "--hook-json") {
				return emitHook(m, name, "SessionStart", nil, err, stdout, stderr)
			}
			return emit(m, name, wantJSON, nil, err, stdout, stderr)
		} else if warn != "" {
			gateWarnings = []string{warn}
			if !wantJSON {
				fmt.Fprintln(stderr, "warning:", warn)
			}
		}
	}

	switch name {
	case "version", "manifest":
		for _, a := range rest {
			if a != "--json" {
				fmt.Fprintf(stderr, "usage: bops %s [--json]\n", name)
				return exitUsage
			}
		}
	}
	switch name {
	case "version":
		return cmdVersion(ctx, m, wantJSON, stdout, stderr)
	case "manifest":
		stdout.Write(bops.ManifestJSON)
		return exitVerdict
	case "gen":
		return cmdGen(m, rest, stdout, stderr)
	case "serve-mcp":
		return cmdServeMCP(m, rest, os.Stdin, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout, m)
		return exitVerdict
	case "next":
		return cmdNext(ctx, m, rest, wantJSON, stdout, stderr)
	case "claim":
		return cmdClaim(ctx, m, rest, wantJSON, stdout, stderr)
	case "land":
		return cmdLand(ctx, m, rest, wantJSON, stdout, stderr)
	case "sweep":
		return cmdSweep(ctx, m, rest, wantJSON, stdout, stderr)
	case "note":
		return cmdNote(ctx, m, rest, wantJSON, stdout, stderr)
	case "release":
		return cmdRelease(ctx, m, rest, wantJSON, stdout, stderr)
	case "sync":
		return cmdSync(ctx, m, rest, wantJSON, stdout, stderr)
	case "lint":
		return cmdLint(ctx, m, rest, wantJSON, stdout, stderr)
	case "stale":
		return cmdStale(ctx, m, rest, wantJSON, stdout, stderr)
	case "reap":
		return cmdReap(ctx, m, rest, wantJSON, stdout, stderr)
	case "session-start":
		return cmdSessionStart(ctx, m, rest, wantJSON, stdout, stderr)
	case "doctor":
		return cmdDoctor(ctx, m, rest, wantJSON, stdout, stderr)
	case "gates":
		return cmdGates(ctx, m, rest, wantJSON, stdout, stderr)
	case "deps":
		return cmdDeps(ctx, m, rest, wantJSON, stdout, stderr)
	}
	if _, ok := m.Lookup(name); ok {
		fmt.Fprintf(stderr, "bops %s: specified in contract %d but not yet implemented (did NOT run)\n", name, m.Contract)
		return exitCantRun
	}
	fmt.Fprintf(stderr, "bops: unknown command %q\n", name)
	usage(stderr, m)
	return exitUsage
}

// budget bounds a whole command. Each git and bd call has its own, shorter
// limit, so a command that uses the network has room to finish. Cancelling a
// push partway would leave its outcome unknown.
func budget(name string) time.Duration {
	switch name {
	case "sweep": // one forge call per landed bead
		return 5 * time.Minute
	case "reap": // a takeover and a release per stale claim
		return 5 * time.Minute
	case "sync":
		return 2 * time.Minute
	case "claim", "release", "note", "land", "session-start", "doctor":
		return time.Minute
	}
	return 30 * time.Second
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
	fmt.Fprintf(w, "  %-14s %s\n  %-14s %s\n", "manifest", "print the embedded commands.json", "gen <target>", "render a binding with [--out <dir>] ("+strings.Join(gen.Targets(), ", ")+"; mcp takes --enable-writes)")
	fmt.Fprintf(w, "  %-14s %s\n", "serve-mcp", "serve the MCP tools on stdio, read-only unless --enable-writes")
}

type versionOut struct {
	Contract int    `json:"contract"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	Bops     string `json:"bops"`
	Bd       string `json:"bd"`
	Backend  string `json:"backend"`
	// Revision, Modified and RevisionTime name the commit this binary was built
	// from (and its commit time), when the go command recorded one (bops-y8x).
	Revision     string `json:"revision"`
	Modified     bool   `json:"modified"`
	RevisionTime string `json:"revision_time"`
	Warning      string `json:"warning,omitempty"`
}

// resolveVersion prefers a version stamped by ldflags, then the module
// version `go install` records, then "dev".
func resolveVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "dev" || info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return stamped
	}
	return info.Main.Version
}

func cmdVersion(ctx context.Context, m *manifest.Manifest, asJSON bool, stdout, stderr *os.File) int {
	info, _ := debug.ReadBuildInfo()
	b := vcsOf(info)
	out := versionOut{Contract: m.Contract, Command: "version", OK: true, Bops: resolveVersion(version, info), Revision: b.Revision, Modified: b.Modified, RevisionTime: b.Time}
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
		fmt.Fprintf(stdout, "bops=%s\ncontract=%d\nbd=%s\nbackend=%s\nrevision=%s\nmodified=%t\nrevision_time=%s\n", out.Bops, out.Contract, out.Bd, out.Backend, out.Revision, out.Modified, out.RevisionTime)
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
	target, out, writes := rest[0], "", false
	for args := rest[1:]; len(args) > 0; args = args[1:] {
		switch a := args[0]; {
		case a == "--out" && len(args) > 1:
			out, args = args[1], args[1:]
		case strings.HasPrefix(a, "--out="):
			out = strings.TrimPrefix(a, "--out=")
		case a == "--enable-writes" && target == "mcp":
			writes = true
		default:
			fmt.Fprintln(stderr, "usage: bops gen <target> [--out <dir>]   (mcp also takes --enable-writes)")
			return exitUsage
		}
	}
	g, ok := gen.Lookup(target)
	if writes {
		g = func(m *manifest.Manifest) ([]gen.File, error) { return gen.MCP(m, true) }
	}
	if !ok {
		fmt.Fprintf(stderr, "bops gen: unknown target %q (have: %s)\n", target, strings.Join(gen.Targets(), ", "))
		return exitUsage
	}
	// The agent skills name the repo's branch_pattern, read from the
	// .bops.json of the repo gen runs in (bops-onn).
	if target == "cc" || target == "codex" {
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintln(stderr, "bops gen:", err)
			return exitCantRun
		}
		o := gen.Options{Branch: cfg.BranchPattern}
		if target == "cc" {
			g = func(m *manifest.Manifest) ([]gen.File, error) { return gen.CCWith(m, o) }
		} else {
			g = func(m *manifest.Manifest) ([]gen.File, error) { return gen.CodexWith(m, o) }
		}
	}
	files, err := g(m)
	if err != nil {
		fmt.Fprintln(stderr, "bops gen:", err)
		return exitCantRun
	}
	if out == "" {
		if len(files) != 1 {
			fmt.Fprintf(stderr, "bops gen %s: writes %d files; pass --out <dir>\n", target, len(files))
			return exitUsage
		}
		stdout.Write(files[0].Data)
		return exitVerdict
	}
	for _, f := range files {
		p := filepath.Join(out, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(stderr, "bops gen:", err)
			return exitCantRun
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			fmt.Fprintln(stderr, "bops gen:", err)
			return exitCantRun
		}
		fmt.Fprintln(stdout, p)
	}
	return exitVerdict
}
