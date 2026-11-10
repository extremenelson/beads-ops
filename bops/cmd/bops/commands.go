package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// envelope is the common result wrapper (SPEC §4).
type envelope struct {
	Contract int    `json:"contract"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// emit writes the result in JSON or key=value form and maps errors to exit
// codes: nil → 0; ErrPrecondition and bd errors → 1.
func emit(m *manifest.Manifest, cmd string, asJSON bool, result any, err error, stdout, stderr *os.File) int {
	env := envelope{Contract: m.Contract, Command: cmd, OK: err == nil}
	if err != nil {
		env.Error = err.Error()
		fmt.Fprintf(stderr, "bops %s: %v (did NOT run to completion)\n", cmd, err)
	}
	if asJSON {
		merged := map[string]any{}
		eb, _ := json.Marshal(env)
		_ = json.Unmarshal(eb, &merged)
		if result != nil && !reflect.ValueOf(result).IsNil() {
			rb, _ := json.Marshal(result)
			_ = json.Unmarshal(rb, &merged)
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(merged)
	} else {
		fmt.Fprintf(stdout, "ok=%t\n", env.OK)
		if result != nil && !reflect.ValueOf(result).IsNil() {
			printKV(stdout, result)
		}
	}
	if err != nil {
		return exitCantRun
	}
	return exitVerdict
}

// printKV flattens a result one level: scalars as key=value, slices as
// key=comma-list (ids when elements are objects with an id), objects skipped.
func printKV(w *os.File, v any) {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch x := m[k].(type) {
		case []any:
			var parts []string
			for _, e := range x {
				if o, ok := e.(map[string]any); ok {
					if id, ok := o["id"].(string); ok {
						parts = append(parts, id)
						continue
					}
				}
				parts = append(parts, fmt.Sprint(e))
			}
			fmt.Fprintf(w, "%s=%s\n", k, strings.Join(parts, ","))
		case map[string]any:
			// nested objects only in JSON mode
		default:
			fmt.Fprintf(w, "%s=%v\n", k, x)
		}
	}
}

func newEnv(ctx context.Context) (*intent.Env, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	run, err := runstate.Load(root)
	if err != nil {
		return nil, fmt.Errorf("run state: %w", err)
	}
	if p := os.Getenv("BOPS_POOL"); p != "" {
		run.Pool = p
	}
	if c := os.Getenv("BOPS_CAPS"); c != "" {
		run.Caps = strings.Split(c, ",")
	}
	return &intent.Env{BD: bdx.Exec{}, Run: run}, nil
}

func saveRun(env *intent.Env) {
	if root, err := os.Getwd(); err == nil {
		_ = env.Run.Save(root)
	}
}

func cmdNext(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 3, "candidates to return")
	inc := fs.Bool("include-unworkable", false, "also return excluded candidates with why_not")
	pool := fs.String("pool", "", "serve only pool:<pool>")
	caps := fs.String("caps", "", "comma-separated capabilities this run offers")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	env, err := newEnv(ctx)
	if err != nil {
		return emit(m, "next", asJSON, nil, err, stdout, stderr)
	}
	if *pool != "" {
		env.Run.Pool = *pool
	}
	if *caps != "" {
		env.Run.Caps = strings.Split(*caps, ",")
	}
	out, err := intent.Next(ctx, env, intent.NextOpts{N: *n, IncludeUnworkable: *inc})
	return emit(m, "next", asJSON, out, err, stdout, stderr)
}

func cmdClaim(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "claim even if assigned")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bops claim <id> [--force]")
		return exitUsage
	}
	env, err := newEnv(ctx)
	if err != nil {
		return emit(m, "claim", asJSON, nil, err, stdout, stderr)
	}
	out, err := intent.Claim(ctx, env, fs.Arg(0), *force)
	saveRun(env)
	return emit(m, "claim", asJSON, out, err, stdout, stderr)
}

func cmdRelease(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "", strings.Join(intent.ReleaseReasons, "|"))
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *reason == "" {
		fmt.Fprintln(stderr, "usage: bops release <id> --reason "+strings.Join(intent.ReleaseReasons, "|"))
		return exitUsage
	}
	env, err := newEnv(ctx)
	if err != nil {
		return emit(m, "release", asJSON, nil, err, stdout, stderr)
	}
	out, err := intent.Release(ctx, env, fs.Arg(0), *reason)
	saveRun(env)
	return emit(m, "release", asJSON, out, err, stdout, stderr)
}

func cmdSync(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noPush := fs.Bool("no-push", false, "pull and heartbeat only")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	env, err := newEnv(ctx)
	if err != nil {
		return emit(m, "sync", asJSON, nil, err, stdout, stderr)
	}
	out, err := intent.Sync(ctx, env, *noPush)
	return emit(m, "sync", asJSON, out, err, stdout, stderr)
}

func cmdLint(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strict := fs.Bool("strict", false, "promote warnings to failures")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bops lint <id> [--strict]")
		return exitUsage
	}
	env, err := newEnv(ctx)
	if err != nil {
		return emit(m, "lint", asJSON, nil, err, stdout, stderr)
	}
	raw, err := env.BD.Run(ctx, "show", fs.Arg(0), "--json")
	if err != nil {
		return emit(m, "lint", asJSON, nil, err, stdout, stderr)
	}
	issue, err := ledger.DecodeIssue(raw)
	if err != nil {
		return emit(m, "lint", asJSON, nil, err, stdout, stderr)
	}
	caps := map[string]bool{}
	for _, c := range env.Run.Caps {
		caps[c] = true
	}
	res := intent.Lint(issue, intent.LintEnv{Pool: env.Run.Pool, Caps: caps, Now: time.Now().UTC(), StaleAfter: 0, Strict: *strict})
	return emit(m, "lint", asJSON, &res, nil, stdout, stderr)
}
