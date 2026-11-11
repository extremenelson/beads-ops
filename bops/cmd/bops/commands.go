package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/forge"
	"github.com/extremenelson/beads-ops/bops/internal/intent"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// envelope is the common result wrapper (SPEC §4).
type envelope struct {
	Contract int    `json:"contract"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
}

// verdict is a result that can be a successful run with a negative answer.
type verdict interface{ Negative() bool }

// emit writes the result in JSON or key=value form and picks the exit code:
// nil → 0; a negative verdict → 3 with ok:false; any error → 1.
func emit(m *manifest.Manifest, cmd string, asJSON bool, result any, err error, stdout, stderr *os.File) int {
	negative := false
	if v, ok := result.(verdict); ok && err == nil {
		negative = v.Negative()
	}
	env := envelope{Contract: m.Contract, Command: cmd, OK: err == nil && !negative}
	if err != nil {
		env.Error = lock.Scrub(err.Error())
		env.Code = lock.CodeOf(err)
		fmt.Fprintf(stderr, "bops %s: %s (did NOT run to completion)\n", cmd, env.Error)
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
	switch {
	case err != nil:
		return exitCantRun
	case negative:
		return exitNegative
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

// parseAnyOrder lets flags appear before or after the bead id. The flag
// package alone stops at the first non-flag argument.
func parseAnyOrder(fs *flag.FlagSet, args []string) error {
	positional := []string{}
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return fs.Parse(append([]string{"--"}, positional...))
}

func newEnv(mode runstate.Mode) (*intent.Env, *runstate.Store, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	store, err := runstate.Open(root, mode)
	if err != nil {
		return nil, nil, fmt.Errorf("run state: %w", err)
	}
	run := store.Run()
	if p := os.Getenv("BOPS_POOL"); p != "" {
		run.Pool = p
	}
	if c := os.Getenv("BOPS_CAPS"); c != "" {
		run.Caps = strings.Split(c, ",")
	}
	l := &lock.Lock{Git: lock.Exec{Dir: root}, Journal: store, Run: run.ID, Actor: run.Actor}
	return &intent.Env{BD: bdx.Exec{}, Run: run, Store: store, Lock: l}, store, nil
}

func cmdNext(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 3, "candidates to return")
	inc := fs.Bool("include-unworkable", false, "also return excluded candidates with why_not")
	pool := fs.String("pool", "", "serve only pool:<pool>")
	caps := fs.String("caps", "", "comma-separated capabilities this run offers")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops next [--n 3] [--pool <name>] [--caps a,b] [--include-unworkable]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "next", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
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
	force := fs.Bool("force", false, "take over an expired claim or an orphan")
	resume := fs.Bool("resume", false, "take back a landing bead for PR rework")
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 || (*force && *resume) {
		fmt.Fprintln(stderr, "usage: bops claim <id> [--force | --resume]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "claim", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	var out *intent.ClaimOut
	if *resume {
		out, err = intent.ClaimResume(ctx, env, fs.Arg(0))
	} else {
		out, err = intent.Claim(ctx, env, fs.Arg(0), *force)
	}
	return emit(m, "claim", asJSON, out, err, stdout, stderr)
}

// noteText reads the note body. It is never taken from an argument.
func noteText(file string, stdin io.Reader, stdinIsTerminal bool) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		return string(b), err
	}
	if stdinIsTerminal {
		return "", errors.New("no note text: pipe it to stdin or use --file")
	}
	b, err := io.ReadAll(stdin)
	return string(b), err
}

func cmdNote(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "read the note from this file instead of stdin")
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bops note <id> [--file <path>]   (text from stdin or --file)")
		return exitUsage
	}
	text, err := noteText(*file, os.Stdin, isTerminal(os.Stdin))
	if err != nil {
		fmt.Fprintln(stderr, "bops note:", err)
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "note", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Note(ctx, env, fs.Arg(0), text)
	return emit(m, "note", asJSON, out, err, stdout, stderr)
}

func cmdLand(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ref := fs.String("ref", "", "URL of the merge request")
	sha := fs.String("sha", "", "commit the work landed as")
	verify := fs.Bool("verify", false, "check the ref's commit on the forge first")
	forgeName := fs.String("forge", "auto", "auto|github|gitlab|none (used with --verify)")
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 || *ref == "" || *sha == "" {
		fmt.Fprintln(stderr, "usage: bops land <id> --ref <url> --sha <sha> [--verify] [--forge auto|github|gitlab|none]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "land", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	opts := intent.LandOpts{Ref: *ref, SHA: *sha}
	if *verify {
		if opts.Verify, err = forge.Select(ctx, *forgeName, forge.Exec); err != nil {
			fmt.Fprintln(stderr, "bops land:", err)
			return exitUsage
		}
	}
	out, err := intent.Land(ctx, env, fs.Arg(0), opts)
	return emit(m, "land", asJSON, out, err, stdout, stderr)
}

func cmdSweep(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", true, "report what would close without closing it")
	forgeName := fs.String("forge", "auto", "auto|github|gitlab|none")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops sweep [--dry-run=false] [--forge auto|github|gitlab|none]")
		return exitUsage
	}
	f, err := forge.Select(ctx, *forgeName, forge.Exec)
	if err != nil {
		fmt.Fprintln(stderr, "bops sweep:", err)
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "sweep", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Sweep(ctx, env, f, *dryRun)
	return emit(m, "sweep", asJSON, out, err, stdout, stderr)
}

func cmdRelease(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "", strings.Join(intent.ReleaseReasons, "|"))
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 || *reason == "" {
		fmt.Fprintln(stderr, "usage: bops release <id> --reason "+strings.Join(intent.ReleaseReasons, "|"))
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "release", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Release(ctx, env, fs.Arg(0), *reason)
	return emit(m, "release", asJSON, out, err, stdout, stderr)
}

func cmdSync(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noPush := fs.Bool("no-push", false, "pull and heartbeat only")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops sync [--no-push]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "sync", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Sync(ctx, env, *noPush)
	return emit(m, "sync", asJSON, out, err, stdout, stderr)
}

func cmdSessionStart(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("session-start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops session-start")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "session-start", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.SessionStart(ctx, env)
	return emit(m, "session-start", asJSON, out, err, stdout, stderr)
}

// cmdDoctor checks origin, read and push access, and lock support. It needs no
// ledger and no run state. A failed check exits 1 with the first failure's
// code, and the document still lists every check.
func cmdDoctor(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops doctor")
		return exitUsage
	}
	root, err := os.Getwd()
	if err != nil {
		return emit(m, "doctor", asJSON, nil, err, stdout, stderr)
	}
	l := &lock.Lock{Git: lock.Exec{Dir: root}, Run: "doctor", Actor: "bops-doctor"}
	out := l.Doctor(ctx)
	for _, c := range out.Checks {
		if c.Status == lock.CheckFail {
			err = &lock.Error{Code: c.Code, Err: fmt.Errorf("%s failed: %s; next: %s", strings.Join(out.Failed, ", "), c.Detail, c.Next)}
			break
		}
	}
	return emit(m, "doctor", asJSON, out, err, stdout, stderr)
}

func cmdStale(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("stale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	older := fs.Duration("older-than", 2*time.Hour, "idle longer than this is stale")
	all := fs.Bool("all-machines", true, "include claims made on other machines")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *older <= 0 {
		fmt.Fprintln(stderr, "usage: bops stale [--older-than 2h] [--all-machines=false]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "stale", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Stale(ctx, env, intent.StaleOpts{OlderThan: *older, AllMachines: *all})
	return emit(m, "stale", asJSON, out, err, stdout, stderr)
}

func cmdLint(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strict := fs.Bool("strict", false, "promote warnings to failures")
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bops lint <id> [--strict]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "lint", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
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
