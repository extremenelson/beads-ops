// Package runstate manages per-machine, per-repo state that must never
// reach the ledger (SPEC.md §3.4): the current run id, pool, capabilities
// and the ids this run holds.
package runstate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// Run is the persisted state for one machine+repo.
type Run struct {
	ID        string    `json:"id"`
	Pool      string    `json:"pool,omitempty"`
	Caps      []string  `json:"caps,omitempty"`
	Machine   string    `json:"machine"`
	Actor     string    `json:"actor"`
	StartedAt time.Time `json:"started_at"`
	Held      []string  `json:"held,omitempty"` // bead ids claimed by this run
}

// Dir returns the state directory for the repo rooted at repoRoot.
func Dir(repoRoot string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(abs))
	return filepath.Join(base, "bops", hex.EncodeToString(h[:8])), nil
}

// Load reads run.json, creating a fresh run if none exists.
func Load(repoRoot string) (*Run, error) {
	dir, err := Dir(repoRoot)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if errors.Is(err, os.ErrNotExist) {
		r := &Run{ID: NewID(), Machine: hostname(), Actor: Actor(), StartedAt: time.Now().UTC()}
		return r, r.Save(repoRoot)
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Actor == "" {
		r.Actor = Actor()
	}
	return &r, nil
}

// Save writes run.json atomically.
func (r *Run) Save(repoRoot string) error {
	dir, err := Dir(repoRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	tmp := filepath.Join(dir, "run.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "run.json"))
}

// Hold records that this run holds id.
func (r *Run) Hold(id string) {
	for _, h := range r.Held {
		if h == id {
			return
		}
	}
	r.Held = append(r.Held, id)
}

// Drop forgets id.
func (r *Run) Drop(id string) {
	out := r.Held[:0]
	for _, h := range r.Held {
		if h != id {
			out = append(out, h)
		}
	}
	r.Held = out
}

// Actor resolves the identity written to the ledger: BEADS_ACTOR, then
// git user.name, then the OS user.
func Actor() string {
	if a := os.Getenv("BEADS_ACTOR"); a != "" {
		return a
	}
	if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func hostname() string {
	if m := os.Getenv("BOPS_MACHINE"); m != "" {
		return m
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// NewID returns a 26-char, time-ordered, Crockford-base32 identifier
// (ULID layout: 48-bit ms timestamp + 80 random bits) with no dependency.
func NewID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	_, _ = rand.Read(b[6:])
	// 128 bits -> 26 base32 chars (130 bits, top 2 bits zero).
	var out [26]byte
	var acc uint32
	var nbits uint
	j := 25
	for i := 15; i >= 0; i-- {
		acc |= uint32(b[i]) << nbits
		nbits += 8
		for nbits >= 5 {
			out[j] = alphabet[acc&31]
			j--
			acc >>= 5
			nbits -= 5
		}
	}
	for j >= 0 {
		out[j] = alphabet[acc&31]
		acc >>= 5
		j--
	}
	return string(out[:])
}
