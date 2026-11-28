package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/config"
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
	env, negative := wrap(m, cmd, result, err, stderr)
	if asJSON {
		merged := document(env, result)
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

// wrap builds the envelope for a result and reports a failed run on stderr.
func wrap(m *manifest.Manifest, cmd string, result any, err error, stderr io.Writer) (envelope, bool) {
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
	return env, negative
}

// document merges the envelope and the result into one JSON object.
func document(env envelope, result any) map[string]any {
	merged := map[string]any{}
	eb, _ := json.Marshal(env)
	_ = json.Unmarshal(eb, &merged)
	if result != nil && !reflect.ValueOf(result).IsNil() {
		rb, _ := json.Marshal(result)
		_ = json.Unmarshal(rb, &merged)
	}
	if len(gateWarnings) > 0 {
		ws, _ := merged["warnings"].([]any)
		for _, w := range gateWarnings {
			ws = append(ws, w)
		}
		merged["warnings"] = ws
	}
	return merged
}

// emitHook writes a SessionStart document as Claude Code hook output: the
// harness parses JSON on stdout as hook control and injects only
// hookSpecificOutput.additionalContext, so bare JSON reaches the model as
// nothing. The context is the document without prime, as indented JSON, then
// the prime text verbatim, which is markdown and reads badly as an escaped
// string. It always exits 0, because Claude Code ignores stdout on any other
// exit code; a failed sync shows as ok:false inside the context instead.
func emitHook(m *manifest.Manifest, cmd, event string, result any, err error, stdout, stderr io.Writer) int {
	env, _ := wrap(m, cmd, result, err, stderr)
	doc := document(env, result)
	prime, _ := doc["prime"].(string)
	delete(doc, "prime")
	b, _ := json.MarshalIndent(doc, "", "  ")
	text := "bops " + cmd + ":\n" + string(b)
	if prime != "" {
		text += "\n\n" + prime
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}}
	_ = json.NewEncoder(stdout).Encode(out)
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
		case []any: // items are cleaned: ids from ref names and warnings from command output can hold line breaks

			var parts []string
			for _, e := range x {
				if o, ok := e.(map[string]any); ok {
					if id, ok := o["id"].(string); ok {
						parts = append(parts, cleanText(id))
						continue
					}
				}
				parts = append(parts, cleanText(fmt.Sprint(e)))
			}
			fmt.Fprintf(w, "%s=%s\n", k, strings.Join(parts, ","))
		case map[string]any:
			// nested objects only in JSON mode
		default:
			fmt.Fprintf(w, "%s=%v\n", k, x)
		}
	}
}

// flagSetMade sees each command's FlagSet as it is made. The flags are defined
// before any parse, so a test that runs a command with -h can read every flag
// it takes and compare them with the manifest.
var flagSetMade = func(*flag.FlagSet) {}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	flagSetMade(fs)
	return fs
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

// commandName is the command run is serving, for what an Env records.
var commandName string

func newEnv(mode runstate.Mode) (*intent.Env, *runstate.Store, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	store, err := runstate.Open(root, mode)
	if err != nil {
		return nil, nil, fmt.Errorf("run state: %w", err)
	}
	cfg, err := configAt(store.Worktree()) // flags override it per command
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	run := store.Run()
	if cfg.Pool != "" {
		run.Pool = cfg.Pool
	}
	if cfg.Caps != nil {
		run.Caps = cfg.Caps
	}
	l := &lock.Lock{Git: lock.Exec{Dir: root}, Journal: store, Run: run.ID, Actor: run.Actor, Machine: run.Machine, Remote: cfg.LockRemote}
	return &intent.Env{BD: bdx.Exec{}, Run: run, Store: store, Lock: l, LockOff: cfg.LockOff(), StaleAfter: cfg.StaleAfter(), HoldFor: cfg.HoldFor(), BlockingLabels: cfg.BlockingLabels, BeadWait: beadWait, Command: commandName}, store, nil
}

// beadWait is how long a command waits for a bead another bops process on
// this machine is working on. It is well inside every command's budget.
const beadWait = 10 * time.Second

func cmdNext(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("next", stderr)
	n := fs.Int("n", 3, "candidates to return")
	inc := fs.Bool("include-unworkable", false, "also return excluded candidates with why_not")
	avoid := fs.Bool("avoid-conflicts", false, "exclude candidates whose touches: paths overlap a live claim's")
	pool := fs.String("pool", "", "serve only pool:<pool>")
	caps := fs.String("caps", "", "comma-separated capabilities this run offers")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops next [--n 3] [--pool <name>] [--caps a,b] [--include-unworkable] [--avoid-conflicts]")
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
	out, err := intent.Next(ctx, env, intent.NextOpts{N: *n, IncludeUnworkable: *inc, AvoidConflicts: *avoid})
	return emit(m, "next", asJSON, out, err, stdout, stderr)
}

func cmdClaim(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("claim", stderr)
	force := fs.Bool("force", false, "take over an expired claim or an orphan")
	resume := fs.Bool("resume", false, "take back a landing bead for PR rework")
	next := fs.Bool("next", false, "claim the first free bead next offers")
	fs.Bool("json", false, "")
	err := parseAnyOrder(fs, args)
	if err == nil && *next && (fs.NArg() != 0 || *force || *resume) {
		err = errors.New("--next takes no id, --force or --resume")
	}
	if err != nil || (!*next && fs.NArg() != 1) || (*force && *resume) {
		fmt.Fprintln(stderr, "usage: bops claim <id> [--force | --resume] | bops claim --next")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "claim", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	var out *intent.ClaimOut
	switch {
	case *next:
		out, err = intent.ClaimNext(ctx, env)
	case *resume:
		out, err = intent.ClaimResume(ctx, env, fs.Arg(0))
	default:
		out, err = intent.Claim(ctx, env, fs.Arg(0), *force)
	}
	code := emit(m, "claim", asJSON, out, err, stdout, stderr)
	if !asJSON && out != nil {
		previousDetails(stdout, out.Previous)
	}
	return code
}

// previousDetails prints the previous attempt of a forced claim, which
// printKV skips as a nested object. Its text comes from the ledger.
func previousDetails(w io.Writer, p *intent.Previous) {
	if p == nil {
		return
	}
	line := func(s string) string { return cleanText(orDashText(strings.Join(strings.Fields(s), " "))) }
	fmt.Fprintf(w, "previous_holder=%s (run %s, idle %dm)\n", line(p.Actor), line(p.Run), p.AgeMinutes)
	fmt.Fprintf(w, "previous_last_note=%s\n", line(p.LastNote))
	if p.NoteRun != "" && p.NoteRun != p.Run {
		fmt.Fprintf(w, "previous_note_run=%s\n", line(p.NoteRun))
	}
	fmt.Fprintf(w, "previous_branch=%s\n", line(p.Branch))
	fmt.Fprintf(w, "previous_mr=%s\n", line(p.MR))
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

// opIDUsage describes --op-id on note, release and land.
const opIDUsage = "name this write, so running it again is a no-op that reports the earlier result (default: derived from the run and content)"

func cmdNote(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("note", stderr)
	file := fs.String("file", "", "read the note from this file instead of stdin")
	opID := fs.String("op-id", "", opIDUsage)
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bops note <id> [--file <path>] [--op-id <id>]   (text from stdin or --file)")
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
	out, err := intent.NoteOp(ctx, env, fs.Arg(0), text, *opID)
	return emit(m, "note", asJSON, out, err, stdout, stderr)
}

func cmdRenew(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("renew", stderr)
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: bops renew <id> [<id>...]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "renew", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Renew(ctx, env, fs.Args())
	code := emit(m, "renew", asJSON, out, err, stdout, stderr)
	if !asJSON && out != nil {
		renewDetails(stdout, out)
	}
	return code
}

// renewDetails prints each bead's outcome, which printKV's id list leaves out.
func renewDetails(w io.Writer, out *intent.RenewOut) {
	for _, b := range out.Beads {
		line := cleanText(b.ID) + " " + b.Outcome
		if b.Reason != "" {
			line += ": " + cleanText(b.Reason)
		}
		fmt.Fprintln(w, line)
	}
}

func cmdLand(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("land", stderr)
	ref := fs.String("ref", "", "URL of the merge request")
	sha := fs.String("sha", "", "commit the work landed as")
	verify := fs.Bool("verify", false, "check the ref's commit on the forge first")
	fs.String("forge", "auto", "auto|github|gitlab|none (used with --verify)")
	opID := fs.String("op-id", "", opIDUsage)
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 || *ref == "" || *sha == "" {
		fmt.Fprintln(stderr, "usage: bops land <id> --ref <url> --sha <sha> [--verify] [--forge auto|github|gitlab|none] [--op-id <id>]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "land", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	opts := intent.LandOpts{Ref: *ref, SHA: *sha, OpID: *opID}
	if *verify {
		cfg, err := loadConfig()
		if err != nil {
			return emit(m, "land", asJSON, nil, err, stdout, stderr)
		}
		if opts.Verify, err = selectForge(ctx, fs, cfg, forge.Exec); err != nil {
			fmt.Fprintln(stderr, "bops land:", err)
			return exitUsage
		}
	}
	out, err := intent.Land(ctx, env, fs.Arg(0), opts)
	return emit(m, "land", asJSON, out, err, stdout, stderr)
}

func cmdSweep(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("sweep", stderr)
	dryRun := fs.Bool("dry-run", true, "report what would close without closing it")
	fs.String("forge", "auto", "auto|github|gitlab|none")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops sweep [--dry-run=false] [--forge auto|github|gitlab|none]")
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		return emit(m, "sweep", asJSON, nil, err, stdout, stderr)
	}
	f, err := selectForge(ctx, fs, cfg, forge.Exec)
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

// releaseText reads release's optional text. Unlike a note's, it is read from
// stdin only when asked with --file -: release usually runs with no text, and
// a caller's stdin may be a pipe that never closes (babashka's p/sh leaves it
// open), which an unasked read would wait on forever. Never an argument.
func releaseText(file string, stdin io.Reader) (string, error) {
	var b []byte
	var err error
	switch file {
	case "":
		return "", nil
	case "-":
		b, err = io.ReadAll(stdin)
	default:
		b, err = os.ReadFile(file)
	}
	return string(b), err
}

func cmdRelease(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("release", stderr)
	reason := fs.String("reason", "", strings.Join(intent.ReleaseReasons, "|"))
	file := fs.String("file", "", "optional text for the release comment from this file; - reads stdin")
	opID := fs.String("op-id", "", opIDUsage)
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 || *reason == "" {
		fmt.Fprintln(stderr, "usage: bops release <id> --reason "+strings.Join(intent.ReleaseReasons, "|")+" [--file <path> | --file -] [--op-id <id>]")
		return exitUsage
	}
	text, err := releaseText(*file, os.Stdin)
	if err != nil {
		fmt.Fprintln(stderr, "bops release:", err)
		return exitUsage
	}
	if *file != "" && strings.TrimSpace(text) == "" {
		return emit(m, "release", asJSON, nil, fmt.Errorf("%w: release text is empty", intent.ErrPrecondition), stdout, stderr)
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "release", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.ReleaseOp(ctx, env, fs.Arg(0), *reason, text, *opID)
	return emit(m, "release", asJSON, out, err, stdout, stderr)
}

func cmdSync(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("sync", stderr)
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
	fs := newFlagSet("session-start", stderr)
	fs.Bool("json", false, "")
	hook := fs.Bool("hook-json", false, "print the document as Claude Code SessionStart hook output")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops session-start [--hook-json]")
		return exitUsage
	}
	show := func(out *intent.SessionStartOut, err error) int {
		if *hook {
			return emitHook(m, "session-start", "SessionStart", out, err, stdout, stderr)
		}
		return emit(m, "session-start", asJSON, out, err, stdout, stderr)
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return show(nil, err)
	}
	defer store.Close()
	return show(intent.SessionStart(ctx, env))
}

// worktreeRoot is the top of dir's git worktree, where .bops.json lives, or
// dir itself outside git (doctor then reports the missing remote).
func worktreeRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if top := strings.TrimSpace(string(out)); err == nil && top != "" {
		return top
	}
	return dir
}

// loadConfig loads the configuration for the worktree holding the current
// directory.
func loadConfig() (config.Config, error) {
	root, err := os.Getwd()
	if err != nil {
		return config.Config{}, err
	}
	return configAt(worktreeRoot(root))
}

// configAt loads the configuration for the worktree rooted at dir. Every
// command loads config through here, so an invalid file or BOPS_* variable
// always carries the stable code bad_config (SPEC §4).
func configAt(dir string) (config.Config, error) {
	cfg, err := config.Load(dir, os.Getenv)
	if err != nil {
		return config.Config{}, &lock.Error{Code: lock.CodeBadConfig, Err: err}
	}
	return cfg, nil
}

// selectForge applies SPEC §7's precedence: --forge when given on the
// command line, else the forge config key, else inferred from lock.remote.
func selectForge(ctx context.Context, fs *flag.FlagSet, cfg config.Config, run forge.Runner) (forge.Forge, error) {
	name := cfg.Forge
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "forge" {
			name = f.Value.String()
		}
	})
	return forge.Select(ctx, name, cfg.LockRemote, run)
}

// doctorBD answers doctor's bd calls: the Dolt remote list, and one list of
// every bead when blocking_labels are configured. Tests replace it.
var doctorBD bdx.Runner = bdx.Exec{}

// cmdDoctor checks the lock remote (lock.remote, default origin), read and
// push access, lock support, and that a Dolt remote is on the same
// repository. It needs no run state. It reads the ledger for its Dolt remote
// list and, when blocking_labels are configured, for one list of every bead,
// to warn of a label no bead carries (bops-jmp). With lock.mode none every
// lock check is skipped; the label warning still runs. A failed check exits 1 with the first failure's
// code, and the document still lists every check.
func cmdDoctor(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("doctor", stderr)
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops doctor")
		return exitUsage
	}
	root, err := os.Getwd()
	if err != nil {
		return emit(m, "doctor", asJSON, nil, err, stdout, stderr)
	}
	cfg, err := configAt(worktreeRoot(root))
	if err != nil {
		return emit(m, "doctor", asJSON, nil, err, stdout, stderr)
	}
	var out *lock.Diagnosis
	lockURL := "" // set when the Dolt remote beside the lock answered doctor's checks
	if cfg.LockOff() {
		out = lock.DoctorOff() // nothing to check, and no remote is touched
	} else {
		l := &lock.Lock{Git: lock.Exec{Dir: root}, Run: "doctor", Actor: "bops-doctor", Remote: cfg.LockRemote}
		out = l.Doctor(ctx)
		if out.Add(intent.DoltRemoteCheck(ctx, doctorBD, l)) && len(out.Failed) == 0 { // the ledger must sit beside the lock
			lockURL, _ = l.RemoteURL(ctx)
		}
	}
	if c, w := minBopsCheck(cfg.MinBops, runningVersion()); c != nil {
		out.Add(*c)
	} else if w != "" {
		out.Warnings = append(out.Warnings, w)
	}
	if w := staleBinary(ctx, worktreeRoot(root), runningBuild()); w != "" {
		out.Warnings = append(out.Warnings, w) // a warning: the lock checks still decide the exit
	}
	if c, ok, w := intent.LedgerConflictCheck(root); ok {
		out.Add(c) // recorded by the command whose pull met it; doctor only reads it
	} else if w != "" {
		out.Warnings = append(out.Warnings, w)
	}
	if w := intent.PullFailureWarning(root); w != "" {
		out.Warnings = append(out.Warnings, w) // a claim's failed pull; the claim holds, so not a failed check
	}
	switch {
	case cfg.LockOff(): // a claim still pushes, and doctor touches no remote
	case lockURL == "": // the remote checks failed: its fetch would only wait out the budget
		out.Warnings = append(out.Warnings, "ledger publication not read: the checks above did not confirm a reachable lock remote with its Dolt remote beside it")
	default:
		if p, err := intent.ReadLedgerPublish(ctx, doctorBD, lockURL); err != nil {
			out.Warnings = append(out.Warnings, "ledger publication not read: "+lock.Scrub(err.Error()))
		} else {
			out.Warnings = append(out.Warnings, p.Warnings()...) // unpublished is normal between pushes: a warning
		}
	}
	if u, err := intent.UnmatchedBlockingLabels(ctx, doctorBD, cfg.BlockingLabels); err != nil {
		out.Warnings = append(out.Warnings, "blocking_labels not checked: bd could not list the beads: "+lock.Scrub(err.Error()))
	} else if len(u) > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("blocking_labels %s: no bead carries it, closed ones and gates included, so it keeps nothing out of the pool; check the spelling against `bd label list-all`", strings.Join(u, ", ")))
	}
	for _, c := range out.Checks {
		if c.Status == lock.CheckFail {
			err = &lock.Error{Code: c.Code, Err: fmt.Errorf("%s failed: %s; next: %s", strings.Join(out.Failed, ", "), c.Detail, c.Next)}
			break
		}
	}
	return emit(m, "doctor", asJSON, out, err, stdout, stderr)
}

func cmdStale(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("stale", stderr)
	older := fs.Duration("older-than", 0, "idle longer than this is stale (default stale_minutes)")
	all := fs.Bool("all-machines", true, "include claims made on other machines")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *older < 0 {
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

func cmdStatus(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("status", stderr)
	older := fs.Duration("older-than", 0, "idle longer than this is stale (default stale_minutes)")
	mineOnly := fs.Bool("mine", false, "only claims made on this machine")
	staleOnly := fs.Bool("stale", false, "only stale, orphan and unknown claims")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *older < 0 {
		fmt.Fprintln(stderr, "usage: bops status [--mine] [--stale] [--older-than 2h]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "status", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Status(ctx, env, intent.StatusOpts{OlderThan: *older, Mine: *mineOnly, Stale: *staleOnly})
	code := emit(m, "status", asJSON, out, err, stdout, stderr)
	if !asJSON && out != nil {
		statusTable(stdout, out)
	}
	return code
}

// statusTable prints the board as a table, after printKV's id list.
func statusTable(w io.Writer, out *intent.StatusOut) {
	if len(out.Claims) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tHOLDER\tMACHINE\tRUN\tAGE\tIDLE\tNOTE\tBRANCH\tMR\tTOUCHES")
	for _, c := range out.Claims {
		machine := c.Machine
		if c.SameMachine {
			machine += "*"
		}
		machine = orDashText(machine)
		cells := []string{c.ID, c.State, orDashText(c.Actor), machine, orDashText(c.Run),
			minutesText(c.AgeMinutes), minutesText(c.IdleMinutes), minutesText(c.LastNoteMinutes), orDashText(c.Branch), orDashText(c.MR), orDashText(strings.Join(c.Touches, "; "))}
		for k := range cells {
			cells[k] = cleanText(cells[k]) // a tab, a control rune, becomes ? and cannot shift the columns
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "(* this machine; ages are since the claim, the last sign of life and the last note)")
}

// minutesText is a minute count as 45m, 3h05m or 2d04h, or - when unknown.
func minutesText(p *int) string {
	switch {
	case p == nil:
		return "-"
	case *p < 60:
		return fmt.Sprintf("%dm", *p)
	case *p < 24*60:
		return fmt.Sprintf("%dh%02dm", *p/60, *p%60)
	}
	return fmt.Sprintf("%dd%02dh", *p/(24*60), *p%(24*60)/60)
}

func cmdReap(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("reap", stderr)
	older := fs.Duration("older-than", 0, "idle longer than this is stale (default stale_minutes)")
	dryRun := fs.Bool("dry-run", true, "report what would be released without releasing it")
	only := fs.String("only", "", "comma-separated bead ids, or id@run to pin the holder: act on these only")
	fs.String("forge", "auto", "auto|github|gitlab|none")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *older < 0 {
		fmt.Fprintln(stderr, "usage: bops reap [--older-than 2h] [--dry-run=false] [--only <id>[@<run>],<id>] [--forge auto|github|gitlab|none]")
		return exitUsage
	}
	ids, ok := onlyIDs(fs, *only)
	if !ok {
		// An empty list (a preview that listed nothing, a failed expansion)
		// must not widen into every stale claim.
		fmt.Fprintln(stderr, "bops reap: --only names no bead ids")
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		return emit(m, "reap", asJSON, nil, err, stdout, stderr)
	}
	f, err := selectForge(ctx, fs, cfg, forge.Exec)
	if err != nil {
		fmt.Fprintln(stderr, "bops reap:", err)
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "reap", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Reap(ctx, env, intent.ReapOpts{OlderThan: *older, DryRun: *dryRun, Only: ids, Forge: f})
	code := emit(m, "reap", asJSON, out, err, stdout, stderr)
	if !asJSON && out != nil {
		reapDetails(stdout, out)
	}
	return code
}

func cmdGC(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("gc", stderr)
	dryRun := fs.Bool("dry-run", true, "report what would be deleted without deleting it")
	only := fs.String("only", "", "comma-separated bead ids: act on these only")
	fs.String("forge", "auto", "auto|github|gitlab|none")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bops gc [--dry-run=false] [--only <id>,<id>] [--forge auto|github|gitlab|none]")
		return exitUsage
	}
	ids, ok := onlyIDs(fs, *only)
	if !ok {
		// An empty list (a preview that listed nothing, a failed expansion)
		// must not widen into every ref.
		fmt.Fprintln(stderr, "bops gc: --only names no bead ids")
		return exitUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		return emit(m, "gc", asJSON, nil, err, stdout, stderr)
	}
	f, err := selectForge(ctx, fs, cfg, forge.Exec)
	if err != nil {
		fmt.Fprintln(stderr, "bops gc:", err)
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "gc", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.GC(ctx, env, intent.GCOpts{DryRun: *dryRun, Only: ids, Forge: f})
	code := emit(m, "gc", asJSON, out, err, stdout, stderr)
	if !asJSON && out != nil {
		gcDetails(stdout, out)
	}
	return code
}

// onlyIDs splits --only into bead ids. It reports false when the flag was
// given but names none.
func onlyIDs(fs *flag.FlagSet, only string) ([]string, bool) {
	var ids []string
	for _, id := range strings.Split(only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "only" })
	return ids, !set || len(ids) > 0
}

// gcDetails prints what printKV's id lists leave out: why each ref is or is
// not deleted.
func gcDetails(w io.Writer, out *intent.GCOut) {
	for _, list := range []struct {
		name string
		gs   []intent.GCRef
	}{{"would delete", out.WouldDelete}, {"deleted", out.Deleted}, {"kept", out.Kept}, {"failed", out.Failed}, {"skipped", out.Skipped}} {
		for _, g := range list.gs {
			what := strings.TrimSpace(g.Why + " " + g.State)
			if g.Run != "" {
				what += ", run " + g.Run + " by " + orDashText(g.Actor)
			}
			line := fmt.Sprintf("%s %s", list.name, cleanText(g.ID))
			if what != "" {
				line += " (" + cleanText(what) + ")"
			}
			if g.Detail != "" {
				line += ": " + cleanText(g.Detail)
			}
			fmt.Fprintln(w, line)
		}
	}
}

// cleanText replaces control characters: ledger and ref text can hold
// terminal control sequences.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || hidesText(r) {
			return '?'
		}
		return r
	}, s)
}

// hidesText reports a rune that reorders or hides the text around it: the
// bidi controls, zero-width space, the line and paragraph separators. Joiners
// (U+200C, U+200D), which scripts and emoji need, are left alone.
func hidesText(r rune) bool {
	switch {
	case r == 0x061C, r == 0x200B, r == 0x200E, r == 0x200F, r == 0x2028, r == 0x2029, r == 0xFEFF:
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

func orDashText(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func savedTo(p string) string {
	if p == "" {
		return ""
	}
	return "(note text saved to " + p + ")"
}

// reapDetails prints what printKV's id lists leave out: why each claim was
// kept, and what to do about each one reap could not finish.
func reapDetails(w io.Writer, out *intent.ReapOut) {
	clean := cleanText // ledger and ref text can hold terminal control sequences
	where := func(same bool) string {
		if same {
			return "this machine"
		}
		return "another machine"
	}
	for _, k := range out.Kept {
		fmt.Fprintf(w, "kept %s (%s, idle %dm): %s\n", clean(k.ID), where(k.Same), k.Age, clean(strings.Join(k.Evidence, "; ")))
	}
	for _, d := range out.WouldRelease {
		name := d.ID // id@run, so --only can pin the holder shown here
		if intent.ValidRun(d.Run) {
			name += "@" + d.Run
		}
		fmt.Fprintf(w, "would release %s (%s, %s, idle %dm)\n", clean(name), where(d.Same), clean(d.Actor), d.Age)
	}
	for _, d := range out.Released { // only what needs a follow-up: pending writes, saved text
		if d.Detail != "" || d.SavedTo != "" {
			fmt.Fprintf(w, "released %s: %s\n", clean(d.ID), clean(strings.TrimSpace(d.Detail+" "+savedTo(d.SavedTo))))
		}
	}
	for _, list := range []struct {
		name string
		ds   []intent.ReapDone
	}{{"failed", out.Failed}, {"skipped", out.Skipped}} {
		for _, d := range list.ds {
			fmt.Fprintf(w, "%s %s: %s\n", list.name, clean(d.ID), clean(strings.TrimSpace(d.Outcome+" "+d.Detail+" "+savedTo(d.SavedTo))))
		}
	}
}

func cmdLint(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("lint", stderr)
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
	gates, err := intent.OpenGatesOf(ctx, env, issue.ID)
	if err != nil {
		return emit(m, "lint", asJSON, nil, err, stdout, stderr)
	}
	res := intent.Lint(issue, intent.LintEnv{Pool: env.Run.Pool, Caps: caps, Now: time.Now().UTC(), StaleAfter: 0, HoldFor: env.HoldFor, BlockingLabels: env.BlockingLabels, Strict: *strict,
		OpenGates: map[string][]string{issue.ID: gates}})
	return emit(m, "lint", asJSON, &res, nil, stdout, stderr)
}

func cmdGates(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("gates", stderr)
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bops gates <id>")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "gates", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Gates(ctx, env, fs.Arg(0))
	return emit(m, "gates", asJSON, out, err, stdout, stderr)
}

func cmdDeps(ctx context.Context, m *manifest.Manifest, args []string, asJSON bool, stdout, stderr *os.File) int {
	fs := newFlagSet("deps", stderr)
	transitive := fs.Bool("transitive", false, "the whole closure, not only direct edges")
	path := fs.Bool("path", false, "add the longest chain of open beads ending at the bead")
	engine := fs.String("engine", "bd", "bd, or cte (needs bd in server mode); graph is reserved")
	fs.Bool("json", false, "")
	if err := parseAnyOrder(fs, args); err != nil || fs.NArg() != 1 || !slices.Contains([]string{"bd", "cte", "graph"}, *engine) {
		fmt.Fprintln(stderr, "usage: bops deps <id> [--transitive] [--path] [--engine bd|cte|graph]")
		return exitUsage
	}
	env, store, err := newEnv(runstate.Read)
	if err != nil {
		return emit(m, "deps", asJSON, nil, err, stdout, stderr)
	}
	defer store.Close()
	out, err := intent.Deps(ctx, env, fs.Arg(0), intent.DepsOpts{Transitive: *transitive, Path: *path, Engine: *engine})
	return emit(m, "deps", asJSON, out, err, stdout, stderr)
}
