// Package bdx is the only package that knows how to talk to beads.
//
// v0 shells out to `bd --json`. A later implementation may open the ledger
// once per process through the beads Go library; the Runner interface is the
// seam. Writes never bypass bd (SPEC.md principle 4).
package bdx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes one bd invocation and returns stdout.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ErrNotFound means bd is not on PATH.
var ErrNotFound = errors.New("bd not found on PATH")

// Exec runs the real bd binary.
type Exec struct {
	Bin string // defaults to "bd"
}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := e.Bin
	if bin == "" {
		bin = "bd"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, ErrNotFound
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = GitEnv(os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		// bd reports failures as {"error": ...} on stdout in --json mode.
		var e struct{ Error, Hint string }
		if json.Unmarshal(out.Bytes(), &e) == nil && e.Error != "" {
			msg = e.Error
			if e.Hint != "" {
				msg += " (" + e.Hint + ")"
			}
		}
		return out.Bytes(), fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.Bytes(), nil
}

// foregroundMaintenance keeps git's automatic maintenance from detaching.
// bd syncs through a git remote cache inside the Dolt database, and each
// fetch into it starts `git maintenance run --auto`, which by default
// detaches and can run `git repack -d`. A detached repack deletes packs
// while the next bd command's `git cat-file` reads them, and Dolt then fails
// with "packfile ... index unavailable" (bops-3tm). In the foreground the
// repack finishes before bd exits. maintenance.autoDetach is git >= 2.48;
// older git reads gc.autoDetach, which the newer key falls back to.
var foregroundMaintenance = [][2]string{
	{"maintenance.autoDetach", "false"},
	{"gc.autoDetach", "false"},
}

// GitEnv returns env with foregroundMaintenance added as GIT_CONFIG_*
// entries, after any the caller already set. An unreadable GIT_CONFIG_COUNT
// is left as it is: git refuses it either way, and says why.
func GitEnv(env []string) []string {
	n, at := 0, -1
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			at = i
			if v == "" {
				n = 0
				continue
			}
			c, err := strconv.Atoi(v)
			if err != nil || c < 0 {
				return env
			}
			n = c
		}
	}
	out := make([]string, 0, len(env)+1+2*len(foregroundMaintenance))
	for i, kv := range env {
		if i != at {
			out = append(out, kv)
		}
	}
	for _, kv := range foregroundMaintenance {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, kv[1]))
		n++
	}
	return append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
}

// Fake replays canned responses keyed by the joined argument string.
// Tests use it so no suite needs Dolt.
type Fake struct {
	Responses map[string][]byte
	// Sequences wins over Responses: successive calls with the same key
	// consume successive entries (the last one repeats). Use it when a
	// readback must differ from the read before a write.
	Sequences map[string][][]byte
	Calls     [][]string
	seqPos    map[string]int
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, args ...string) ([]byte, error) {
	f.Calls = append(f.Calls, args)
	key := strings.Join(args, " ")
	if seq, ok := f.Sequences[key]; ok && len(seq) > 0 {
		if f.seqPos == nil {
			f.seqPos = map[string]int{}
		}
		i := f.seqPos[key]
		if i < len(seq)-1 {
			f.seqPos[key] = i + 1
		}
		return seq[i], nil
	}
	if b, ok := f.Responses[key]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bdx.Fake: no response for %q", key)
}

// Version is what `bd version --json` reports (fields we rely on).
type Version struct {
	Version string `json:"version"`
	Backend string `json:"backend,omitempty"`
}

// DetectVersion asks bd for its version. It tolerates non-JSON output from
// older builds by returning the raw first line.
func DetectVersion(ctx context.Context, r Runner) (Version, error) {
	out, err := r.Run(ctx, "version", "--json")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Version{}, err
		}
		// Older bd may not accept --json; fall back to plain.
		out, err = r.Run(ctx, "version")
		if err != nil {
			return Version{}, err
		}
	}
	var v Version
	if json.Unmarshal(out, &v) == nil && v.Version != "" {
		return v, nil
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return Version{Version: line}, nil
}
