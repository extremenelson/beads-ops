// Package config loads bops configuration (SPEC §10).
//
// Layers, lowest to highest precedence:
//
//  1. built-in defaults
//  2. `.bops.json` in the repo root (committed, shared by the team)
//  3. `$XDG_CONFIG_HOME/bops/config.json`, else `~/.config/bops/config.json` (per user)
//  4. `BOPS_*` environment variables
//
// Command-line flags override all of them; that happens in cmd/bops. A layer
// sets only the keys it names, except blocking_labels, to which each layer
// adds, so no layer can drop a label a lower one names. An unknown key, a
// value of the wrong type or an out-of-range value is an error naming the
// file or variable, never silently ignored.
//
// To add a key: add a field to Config, a default in Default if it needs one,
// and one row to keys. The row gives its JSON path, kind and field; the
// environment name is derived from the path.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config is the merged configuration.
type Config struct {
	Pool            string   // pool: serve only pool:<pool>
	Caps            []string // caps[]: capabilities this run offers
	HoldMinutes     int      // hold_minutes: bops:hold cooling-off
	StaleMinutes    int      // stale_minutes: a claim idle this long is stale
	Forge           string   // forge: auto|github|gitlab|none
	LintStrict      bool     // lint.strict: promote warnings to failures
	LintDisable     []string // lint.disable[]: rule ids to skip (loaded, not yet applied)
	NoteRequireNext bool     // note.require_next (loaded, not yet applied)
	LockMode        string   // lock.mode: ref|none; none only from the repo file
	LockRemote      string   // lock.remote: git remote that holds the claim refs
	// BlockingLabels (blocking_labels[]) keep a bead out of next and refuse a
	// plain claim. Unlike every other key, each layer adds to it, so a user
	// file or the environment can never drop a label the repo file names.
	BlockingLabels []string
	// MinBops (min_bops) is the oldest bops release this ledger accepts,
	// MAJOR.MINOR.PATCH without a v; only the repo file may set it.
	MinBops string
}

// Lock modes. LockNone decides claims from the local ledger alone, with no
// git ref and no network: only for a ledger used on one machine.
const (
	LockRef  = "ref"
	LockNone = "none"
)

// LockOff reports whether lock.mode turns the claim lock off.
func (c Config) LockOff() bool { return c.LockMode == LockNone }

// DefaultMinutes is the default for hold_minutes and stale_minutes.
const DefaultMinutes = 120

// Default is the configuration with no files and no environment.
func Default() Config {
	return Config{HoldMinutes: DefaultMinutes, StaleMinutes: DefaultMinutes, Forge: "auto", LockMode: LockRef, LockRemote: "origin"}
}

// StaleAfter is stale_minutes as a duration.
func (c Config) StaleAfter() time.Duration { return time.Duration(c.StaleMinutes) * time.Minute }

// HoldFor is hold_minutes as a duration.
func (c Config) HoldFor() time.Duration { return time.Duration(c.HoldMinutes) * time.Minute }

type kind int

const (
	kString kind = iota
	kStrings
	kMinutes
	kBool
	kForge
	kLockMode
	kRemote
	kLabels
	kMinVersion
)

type key struct {
	path string // JSON path, dotted for nested objects
	kind kind
	ref  func(c *Config) any // pointer to the field
}

var keys = []key{
	{"pool", kString, func(c *Config) any { return &c.Pool }},
	{"caps", kStrings, func(c *Config) any { return &c.Caps }},
	{"hold_minutes", kMinutes, func(c *Config) any { return &c.HoldMinutes }},
	{keyStaleMinutes, kMinutes, func(c *Config) any { return &c.StaleMinutes }},
	{"forge", kForge, func(c *Config) any { return &c.Forge }},
	{"lint.strict", kBool, func(c *Config) any { return &c.LintStrict }},
	{keyLintDisable, kStrings, func(c *Config) any { return &c.LintDisable }},
	{"note.require_next", kBool, func(c *Config) any { return &c.NoteRequireNext }},
	{"lock.mode", kLockMode, func(c *Config) any { return &c.LockMode }},
	{"lock.remote", kRemote, func(c *Config) any { return &c.LockRemote }},
	{"blocking_labels", kLabels, func(c *Config) any { return &c.BlockingLabels }},
	{keyMinBops, kMinVersion, func(c *Config) any { return &c.MinBops }},
}

var (
	forges    = []string{"auto", "github", "gitlab", "none"}
	lockModes = []string{LockRef, LockNone}
)

// lockOffHere refuses lock.mode=none from a layer above the repo file unless
// the repo file already chose it. One machine of a shared ledger turning the
// lock off would silently lose that machine's protection, so the choice is
// the team's, in the committed file. Setting ref is always allowed.
func lockOffHere(c *Config, repoMode string) error {
	if c.LockMode == LockNone && repoMode != LockNone {
		return fmt.Errorf("lock.mode none is accepted only from %s, the committed file every machine reads; set it there if this ledger is used on one machine", RepoFile)
	}
	return nil
}

// lintW15Here refuses W15 in lint.disable from a layer above the repo file
// unless the repo file already disables it. W15 is what blocking_labels
// enforce in next, and each layer only adds to those labels so none can drop
// the repo's; disabling W15 above the repo would undo that (bops-jmp).
func lintW15Here(c *Config, repoDisable []string) error {
	if disablesW15(c.LintDisable) && !disablesW15(repoDisable) {
		return fmt.Errorf("lint.disable W15 is accepted only from %s, the committed file every machine reads: W15 enforces blocking_labels, which no other layer may drop", RepoFile)
	}
	return nil
}

// staleHere refuses a stale_minutes from a layer above the repo file that is
// shorter than the repo file's (or the default, when it sets none). A machine
// with a shorter window takes over other machines' claims sooner than they
// expect, so only the committed file may shorten it; any layer may lengthen
// it, which only makes that machine wait longer (bops-i08).
func staleHere(c *Config, repoStale int) error {
	if c.StaleMinutes < repoStale {
		return fmt.Errorf("stale_minutes %d is shorter than %d, the window %s sets (or the default, if it sets none): a machine with a shorter window takes over other machines' claims sooner, so only %s, the committed file every machine reads, may shorten it; another layer may only lengthen it", c.StaleMinutes, repoStale, RepoFile, RepoFile)
	}
	return nil
}

// keyStaleMinutes is stale_minutes' path, shared by its row in keys and the
// staleHere guard on the environment layer.
const keyStaleMinutes = "stale_minutes"

// disablesW15 reports whether a lint.disable list names W15, however cased or
// spaced, so the guard is not dodged by "w15" or " W15 ".
func disablesW15(ids []string) bool {
	return slices.ContainsFunc(ids, func(id string) bool { return strings.EqualFold(strings.TrimSpace(id), "W15") })
}

// keyLintDisable is lint.disable's path, shared by its row in keys and the
// W15 guard on the environment layer.
const keyLintDisable = "lint.disable"

// remoteName is a plausible git remote name: one refname segment that cannot
// be read as a flag or a URL.
var remoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func validRemote(v string) bool {
	return remoteName.MatchString(v) && !strings.Contains(v, "..") && !strings.HasSuffix(v, ".") && !strings.HasSuffix(v, ".lock")
}

// EnvName is the environment variable for a key path: lint.strict is
// BOPS_LINT_STRICT.
func EnvName(path string) string {
	return "BOPS_" + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// RepoFile is the committed file in the repo root; userRel is the per-user
// file under the config home.
const (
	RepoFile = ".bops.json"
	userRel  = "bops/config.json"
)

// UserPath is where the per-user file lives, or "" when there is no home.
func UserPath(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, filepath.FromSlash(userRel))
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", filepath.FromSlash(userRel))
	}
	return ""
}

// Load merges the layers for the repo whose root is repoRoot. getenv is
// os.Getenv in production. A missing file is not an error.
func Load(repoRoot string, getenv func(string) string) (Config, error) {
	c := Default()
	files := []string{filepath.Join(repoRoot, RepoFile)}
	if u := UserPath(getenv); u != "" {
		files = append(files, u)
	}
	repoMode, repoDisable, repoStale, repoMin := c.LockMode, c.LintDisable, c.StaleMinutes, c.MinBops
	for n, f := range files {
		err := mergeFile(&c, f)
		if n == 0 {
			repoMode, repoDisable, repoStale, repoMin = c.LockMode, c.LintDisable, c.StaleMinutes, c.MinBops
		} else if err == nil {
			err = lockOffHere(&c, repoMode)
			if err == nil {
				err = lintW15Here(&c, repoDisable)
			}
			if err == nil {
				err = staleHere(&c, repoStale)
			}
			if err == nil {
				err = minBopsHere(&c, repoMin)
			}
		}
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", f, err)
		}
	}
	for _, k := range keys {
		name := EnvName(k.path)
		v := getenv(name)
		if v == "" {
			continue
		}
		err := setString(&c, k, v)
		if err == nil && k.kind == kLockMode {
			err = lockOffHere(&c, repoMode)
		}
		if err == nil && k.path == keyLintDisable {
			err = lintW15Here(&c, repoDisable)
		}
		if err == nil && k.path == keyStaleMinutes {
			err = staleHere(&c, repoStale)
		}
		if err == nil && k.path == keyMinBops {
			err = minBopsHere(&c, repoMin)
		}
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", name, err)
		}
	}
	return c, nil
}

func mergeFile(c *Config, path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := strictJSON(b, &top); err != nil {
		return fmt.Errorf("want a JSON object: %w", err)
	}
	flat := map[string]json.RawMessage{}
	if err := flatten("", top, flat); err != nil {
		return err
	}
	byPath := map[string]key{}
	for _, k := range keys {
		byPath[k.path] = k
	}
	names := make([]string, 0, len(flat))
	for p := range flat {
		names = append(names, p)
	}
	sort.Strings(names) // report the same error first every time
	for _, p := range names {
		k, ok := byPath[p]
		if !ok {
			return fmt.Errorf("unknown key %q (have: %s)", p, known())
		}
		if err := setJSON(c, k, flat[p]); err != nil {
			return err
		}
	}
	return nil
}

// flatten turns {"lint":{"strict":true}} into "lint.strict". A section is
// any key that some known key path sits under.
func flatten(prefix string, m map[string]json.RawMessage, out map[string]json.RawMessage) error {
	for name, raw := range m {
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		if isSection(p) {
			var sub map[string]json.RawMessage
			if err := strictJSON(raw, &sub); err != nil || sub == nil {
				return fmt.Errorf("%s: want an object", p)
			}
			if err := flatten(p, sub, out); err != nil {
				return err
			}
			continue
		}
		out[p] = raw
	}
	return nil
}

func isSection(p string) bool {
	for _, k := range keys {
		if strings.HasPrefix(k.path, p+".") {
			return true
		}
	}
	return false
}

func known() string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.path
	}
	return strings.Join(names, ", ")
}

func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data after the object")
	}
	return nil
}

func setJSON(c *Config, k key, raw json.RawMessage) error {
	switch k.kind {
	case kString, kForge, kLockMode, kRemote, kMinVersion:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s: want a string", k.path)
		}
		return setString(c, k, s)
	case kStrings, kLabels:
		var ss []string
		if err := json.Unmarshal(raw, &ss); err != nil || ss == nil {
			return fmt.Errorf("%s: want a list of strings", k.path)
		}
		if k.kind == kLabels {
			return addLabels(c, k, ss)
		}
		*k.ref(c).(*[]string) = ss
	case kMinutes:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil || f != math.Trunc(f) || f > math.MaxInt32 {
			return fmt.Errorf("%s: want a whole number of minutes, got %s", k.path, raw)
		}
		return setMinutes(c, k, int(f))
	case kBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%s: want true or false, got %s", k.path, raw)
		}
		*k.ref(c).(*bool) = v
	}
	return nil
}

// setString applies a value given as text: an environment variable, or a
// JSON string for string-valued keys.
func setString(c *Config, k key, v string) error {
	switch k.kind {
	case kString:
		*k.ref(c).(*string) = v
	case kForge, kLockMode:
		allowed := forges
		if k.kind == kLockMode {
			allowed = lockModes
		}
		for _, f := range allowed {
			if v == f {
				*k.ref(c).(*string) = v
				return nil
			}
		}
		return fmt.Errorf("%s: want one of %s, got %q", k.path, strings.Join(allowed, ", "), v)
	case kRemote:
		if !validRemote(v) {
			return fmt.Errorf("%s: want a git remote name such as origin, got %q", k.path, v)
		}
		*k.ref(c).(*string) = v
	case kMinVersion:
		if !minVersion.MatchString(v) {
			return fmt.Errorf("%s: want a bops release such as 0.5.1, got %q", k.path, v)
		}
		*k.ref(c).(*string) = normalizeMin(v)
	case kStrings, kLabels:
		var ss []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				ss = append(ss, s)
			}
		}
		if k.kind == kLabels {
			return addLabels(c, k, ss)
		}
		*k.ref(c).(*[]string) = ss
	case kMinutes:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("want a whole number of minutes, got %q", v)
		}
		return setMinutes(c, k, n)
	case kBool:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("want true or false, got %q", v)
		}
		*k.ref(c).(*bool) = b
	}
	return nil
}

// bopsLabelPrefixes are the label prefixes bops itself sets: bops:hold and
// bops:landing on a bead's way through, run:<id> on a claim and pool:<name>
// on a pooled bead. A blocking label among them would turn W12's timed
// cool-off into a permanent exclusion, or block every claimed or pooled bead,
// with a second reason that explains neither (bops-jmp).
var bopsLabelPrefixes = []string{"bops:", "run:", "pool:"}

// addLabels adds bead labels to a kLabels key, sorted and without repeats.
// A label is one bd label: printable, with no whitespace or comma, since
// anything else (a zero-width space, a control character) could never match
// one bd stores. bops's own labels are refused.
func addLabels(c *Config, k key, labels []string) error {
	dst := k.ref(c).(*[]string)
	for _, l := range labels {
		if l == "" || strings.IndexFunc(l, func(r rune) bool { return r == ',' || unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
			return fmt.Errorf("%s: want printable bead labels with no space or comma, got %q", k.path, l)
		}
		for _, p := range bopsLabelPrefixes {
			if strings.HasPrefix(l, p) {
				return fmt.Errorf("%s: %q is one of bops's own labels (%s*), which bops sets and clears itself; name a label of the repo's own", k.path, l, p)
			}
		}
		if !slices.Contains(*dst, l) {
			*dst = append(*dst, l)
		}
	}
	sort.Strings(*dst)
	return nil
}

func setMinutes(c *Config, k key, n int) error {
	if n < 1 {
		return fmt.Errorf("%s: want at least 1, got %d", k.path, n)
	}
	*k.ref(c).(*int) = n
	return nil
}
