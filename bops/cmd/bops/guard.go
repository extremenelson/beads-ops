package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/guard"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

// GuardOut is the `guard` output: whether the command holds a raw bd claim
// that the claim lock refuses.
type GuardOut struct {
	Refused  bool     `json:"refused"`
	Matched  string   `json:"matched"`
	Use      string   `json:"use"`
	Reason   string   `json:"reason"`
	Warnings []string `json:"warnings"`
}

// guardMaxInput caps what guard reads; a hook payload past it is allowed.
const guardMaxInput = 1 << 20

// hookPayload is the part of Claude Code's PreToolUse input guard reads.
type hookPayload struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	Cwd string `json:"cwd"`
}

// cmdGuard checks a shell command for bd update --claim, bd ready --claim
// and bd reclaim. It reads the command as text, or with --hook-json Claude
// Code's PreToolUse payload, and refuses those claims while the repo's claim
// lock is on. It fails open: whatever stops it from deciding (unreadable
// input or config) allows the command. With --hook-json it always exits 0
// and prints a deny decision only when it refuses; an allowed command prints
// nothing, so Claude Code's own permission prompt still applies.
func cmdGuard(m *manifest.Manifest, args []string, asJSON bool, stdin io.Reader, stdinTTY bool, stdout, stderr *os.File) (code int) {
	fs := newFlagSet("guard", stderr)
	file := fs.String("file", "", "read the command from this file instead of stdin")
	hook := fs.Bool("hook-json", false, "read Claude Code's PreToolUse payload and answer as its hook")
	fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*hook && *file != "") {
		fmt.Fprintln(stderr, "usage: bops guard [--file <path>] | bops guard --hook-json   (command, or the hook payload, from stdin)")
		if *hook {
			return exitVerdict // a hook that cannot run allows
		}
		return exitUsage
	}
	if *hook {
		defer func() { // fail open, even on a bug
			if r := recover(); r != nil {
				code = exitVerdict
			}
		}()
		cmd, cwd, ok := readHookPayload(stdin)
		if !ok {
			return exitVerdict
		}
		out := guardCheck(cmd, cwd)
		if out.Refused {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": out.Reason,
			}})
		}
		return exitVerdict
	}

	var r io.Reader = stdin
	if *file != "" && *file != "-" {
		f, err := os.Open(*file)
		if err != nil {
			fmt.Fprintln(stderr, "bops guard:", err)
			return exitUsage
		}
		defer f.Close()
		r = f
	} else if *file == "" && stdinTTY {
		fmt.Fprintln(stderr, "bops guard: give the command on stdin or with --file")
		return exitUsage
	}
	b, err := io.ReadAll(io.LimitReader(r, guardMaxInput+1))
	if err != nil {
		return emit(m, "guard", asJSON, nil, err, stdout, stderr)
	}
	if len(b) > guardMaxInput {
		out := &GuardOut{Warnings: []string{"command longer than 1 MiB: not checked, allowed"}}
		return emit(m, "guard", asJSON, out, nil, stdout, stderr)
	}
	wd, _ := os.Getwd()
	return emit(m, "guard", asJSON, guardCheck(string(b), wd), nil, stdout, stderr)
}

// readHookPayload returns the Bash command and directory of a PreToolUse
// payload, or false for anything else.
func readHookPayload(r io.Reader) (cmd, cwd string, ok bool) {
	b, err := io.ReadAll(io.LimitReader(r, guardMaxInput+1))
	if err != nil || len(b) > guardMaxInput {
		return "", "", false
	}
	var p hookPayload
	if json.Unmarshal(b, &p) != nil || p.ToolName != "Bash" || p.ToolInput.Command == "" {
		return "", "", false
	}
	if p.Cwd == "" {
		p.Cwd, _ = os.Getwd()
	}
	return p.ToolInput.Command, p.Cwd, true
}

// guardGitWait bounds the one git call guard makes, so a stuck git cannot
// hold up the agent's Bash call; past it, guard uses dir as the root.
const guardGitWait = 2 * time.Second

// guardRoot is worktreeRoot with a time limit. It also returns the main
// checkout of a linked worktree (the parent of git's common dir), where bd
// finds the ledger when the worktree has no .beads of its own; that is root
// itself in a main checkout or outside git.
func guardRoot(dir string) (root, main string) {
	ctx, cancel := context.WithTimeout(context.Background(), guardGitWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel", "--git-common-dir")
	cmd.WaitDelay = guardGitWait // a child holding stdout cannot outlast the kill
	out, err := cmd.Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || len(lines) != 2 || lines[0] == "" {
		return dir, dir
	}
	root, common := lines[0], lines[1]
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return root, filepath.Dir(common)
}

func hasBeads(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".beads"))
	return err == nil && fi.IsDir()
}

// guardCheck refuses a raw claim in cmd when the worktree holding dir is a
// beads ledger (it, or its main checkout, has .beads) whose claim lock is on. Anything else allows:
// no .beads (not a ledger, so bd would not claim here anyway), lock.mode
// none, or a config it cannot load, the last two with a warning.
func guardCheck(cmd, dir string) *GuardOut {
	out := &GuardOut{Warnings: []string{}}
	hit, found := guard.Check(cmd)
	if !found {
		return out
	}
	out.Matched = hit.Matched
	root, main := guardRoot(dir)
	if !hasBeads(root) && !hasBeads(main) {
		out.Warnings = append(out.Warnings, "no .beads directory in "+root+": not a beads ledger, so the command is allowed")
		return out
	}
	cfg, err := configAt(root)
	if err != nil {
		var le *lock.Error
		if errors.As(err, &le) {
			err = le.Err
		}
		out.Warnings = append(out.Warnings, "config not loaded, so the command is allowed: "+strings.SplitN(err.Error(), "\n", 2)[0])
		return out
	}
	if cfg.LockOff() {
		out.Warnings = append(out.Warnings, lock.OffWarning)
		return out
	}
	out.Refused = true
	out.Use = hit.Use
	out.Reason = fmt.Sprintf("bops guard: `%s` ignores the bops claim lock (it claims on this replica only), so this repo refuses it. Use: %s.", hit.Matched, hit.Use)
	return out
}
