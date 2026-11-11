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
// sets only the keys it names. An unknown key, a value of the wrong type or
// an out-of-range value is an error naming the file or variable, never
// silently ignored.
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
	"sort"
	"strconv"
	"strings"
	"time"
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
}

// DefaultMinutes is the default for hold_minutes and stale_minutes.
const DefaultMinutes = 120

// Default is the configuration with no files and no environment.
func Default() Config {
	return Config{HoldMinutes: DefaultMinutes, StaleMinutes: DefaultMinutes, Forge: "auto"}
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
	{"stale_minutes", kMinutes, func(c *Config) any { return &c.StaleMinutes }},
	{"forge", kForge, func(c *Config) any { return &c.Forge }},
	{"lint.strict", kBool, func(c *Config) any { return &c.LintStrict }},
	{"lint.disable", kStrings, func(c *Config) any { return &c.LintDisable }},
	{"note.require_next", kBool, func(c *Config) any { return &c.NoteRequireNext }},
}

var forges = []string{"auto", "github", "gitlab", "none"}

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
	for _, f := range files {
		if err := mergeFile(&c, f); err != nil {
			return Config{}, fmt.Errorf("config %s: %w", f, err)
		}
	}
	for _, k := range keys {
		name := EnvName(k.path)
		v := getenv(name)
		if v == "" {
			continue
		}
		if err := setString(&c, k, v); err != nil {
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
	case kString, kForge:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s: want a string", k.path)
		}
		return setString(c, k, s)
	case kStrings:
		var ss []string
		if err := json.Unmarshal(raw, &ss); err != nil || ss == nil {
			return fmt.Errorf("%s: want a list of strings", k.path)
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
	case kForge:
		for _, f := range forges {
			if v == f {
				*k.ref(c).(*string) = v
				return nil
			}
		}
		return fmt.Errorf("%s: want one of %s, got %q", k.path, strings.Join(forges, ", "), v)
	case kStrings:
		var ss []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				ss = append(ss, s)
			}
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

func setMinutes(c *Config, k key, n int) error {
	if n < 1 {
		return fmt.Errorf("%s: want at least 1, got %d", k.path, n)
	}
	*k.ref(c).(*int) = n
	return nil
}
