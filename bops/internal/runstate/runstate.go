// Package runstate manages per-machine state that must never reach the
// ledger (SPEC.md §3.4). A clone has one state file and one write lock,
// because all of its worktrees share one ledger database. Each worktree
// has its own run inside that file, so concurrent agents stay distinct.
package runstate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Run is the persisted state for one worktree of a clone.
type Run struct {
	ID        string    `json:"id"`
	Pool      string    `json:"pool,omitempty"`
	Caps      []string  `json:"caps,omitempty"`
	Machine   string    `json:"machine"`
	Actor     string    `json:"actor"`
	StartedAt time.Time `json:"started_at"`
	Held      Held      `json:"held,omitempty"` // the journal of this run's holds
}

// Journal phases. A pushing entry has never been confirmed on the remote, so
// losing it loses nothing. A held entry with no sha predates the lock.
const (
	PhasePushing = "pushing"
	PhaseHeld    = "held"
)

// Entry is one bead in a run's hold journal.
type Entry struct {
	SHA           string `json:"sha,omitempty"`      // the ref value this run last confirmed
	Intended      string `json:"intended,omitempty"` // a ref write whose outcome is not known yet
	Phase         string `json:"phase"`
	LedgerPending bool   `json:"ledger_pending,omitempty"`
	DeletePending bool   `json:"delete_pending,omitempty"`
}

// Legacy reports whether the entry was taken before the lock existed.
func (e Entry) Legacy() bool { return e.Phase == PhaseHeld && e.SHA == "" }

// Held is a run's journal, keyed by bead id. It also decodes the list of ids
// written by versions before the lock.
type Held map[string]Entry

// UnmarshalJSON accepts both the journal and the legacy list.
func (h *Held) UnmarshalJSON(b []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
		var ids []string
		if err := json.Unmarshal(b, &ids); err != nil {
			return err
		}
		*h = Held{}
		for _, id := range ids {
			(*h)[id] = Entry{Phase: PhaseHeld}
		}
		return nil
	}
	var m map[string]Entry
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*h = m
	return nil
}

// stateVersion is written to state.json. A newer file is refused rather than
// rewritten by a binary that would drop what it does not understand.
const stateVersion = 2

// Mode says whether a Store may be saved.
type Mode int

const (
	// Read sees the current state and takes the lock only to record a new run.
	Read Mode = iota
	// Write holds the clone's lock until Close.
	Write
)

const lockStale = 5 * time.Minute

// lockWait is how long Open waits for another bops process. A var for tests.
var lockWait = 30 * time.Second

// Hold is one bead held by some run in this clone.
type Hold struct {
	ID       string
	Run      string
	Worktree string
	Stranded bool // the worktree that took this hold no longer exists
	Entry    Entry
}

type state struct {
	Version int             `json:"version"`
	Runs    map[string]*Run `json:"runs"` // keyed by absolute worktree root
}

// Store is one process's view of a clone's state file.
type Store struct {
	dir      string
	worktree string
	st       state
	run      *Run
	locked   bool
	token    string // proves the lock file on disk is this Store's
}

// locate finds the clone and worktree containing dir. Outside a git
// repository, dir stands in for both.
func locate(dir string) (commonDir, worktree string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	out, err := exec.Command("git", "-C", abs, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel").Output()
	if err != nil {
		return abs, abs, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return abs, abs, nil
	}
	return lines[0], lines[1], nil
}

func stateDir(commonDir string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	h := sha256.Sum256([]byte(commonDir))
	return filepath.Join(base, "bops", hex.EncodeToString(h[:8])), nil
}

// Open loads the state for the clone containing start, creating a run for
// its worktree if there is none. Callers must Close the Store.
func Open(start string, mode Mode) (*Store, error) {
	commonDir, worktree, err := locate(start)
	if err != nil {
		return nil, err
	}
	dir, err := stateDir(commonDir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, worktree: worktree}
	if mode == Write {
		if err := s.lock(); err != nil {
			return nil, err
		}
	}
	if err := s.ensureRun(); err != nil {
		s.Close()
		return nil, err
	}
	s.run = s.st.Runs[worktree]
	if mode == Read {
		s.Close()
	}
	return s, nil
}

func (s *Store) ensureRun() error {
	if err := s.read(); err != nil {
		return err
	}
	if s.st.Runs[s.worktree] != nil {
		return nil
	}
	if !s.locked {
		if err := s.lock(); err != nil {
			return err
		}
		if err := s.read(); err != nil { // another process may have added it
			return err
		}
		if s.st.Runs[s.worktree] != nil {
			return nil
		}
	}
	s.st.Runs[s.worktree] = &Run{ID: NewID(), Machine: hostname(), Actor: Actor(), StartedAt: time.Now().UTC()}
	return s.write()
}

func (s *Store) read() error {
	s.st = state{Runs: map[string]*Run{}}
	b, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return fmt.Errorf("state file %s: %w", filepath.Join(s.dir, "state.json"), err)
	}
	if s.st.Version > stateVersion {
		return fmt.Errorf("state file %s is version %d; this bops understands up to %d, so upgrade it", filepath.Join(s.dir, "state.json"), s.st.Version, stateVersion)
	}
	if s.st.Runs == nil {
		s.st.Runs = map[string]*Run{}
	}
	for _, r := range s.st.Runs {
		if r.Actor == "" {
			r.Actor = Actor()
		}
	}
	return nil
}

// write replaces the state file atomically. The lock must be held.
func (s *Store) write() error {
	s.st.Version = stateVersion
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

// lock takes the clone's write lock. A lock left by a crashed process is
// broken after lockStale. Two waiters that both break the same stale lock
// can, in a very narrow window, both proceed.
func (s *Store) lock() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "lock")
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			s.token = fmt.Sprintf("%d %s\n", os.Getpid(), NewID())
			_, werr := f.WriteString(s.token)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(path)
				return werr
			}
			s.locked = true
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStale {
			aside := path + "." + NewID()
			if os.Rename(path, aside) == nil {
				_ = os.Remove(aside)
			}
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another bops process holds the state lock %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Close releases the write lock if this Store still owns it. A lock broken
// as abandoned may since have been taken by another process.
func (s *Store) Close() {
	if !s.locked {
		return
	}
	s.locked = false
	path := filepath.Join(s.dir, "lock")
	if b, err := os.ReadFile(path); err == nil && string(b) == s.token {
		_ = os.Remove(path)
	}
}

// update applies fn to the freshest state and writes it, taking the lock only
// for that long, so a command never holds it across a network call. A Store
// opened with Write already holds the lock and keeps it.
func (s *Store) update(fn func()) error {
	if !s.locked {
		if err := s.lock(); err != nil {
			return err
		}
		defer s.Close()
		if err := s.refresh(); err != nil {
			return err
		}
	}
	fn()
	return s.write()
}

// refresh re-reads the file while keeping s.run the same pointer, because
// callers hold it. Pool and caps are per-process overrides and stay.
func (s *Store) refresh() error {
	if err := s.read(); err != nil {
		return err
	}
	if s.run == nil {
		return nil
	}
	if fresh := s.st.Runs[s.worktree]; fresh != nil {
		pool, caps := s.run.Pool, s.run.Caps
		*s.run = *fresh
		s.run.Pool, s.run.Caps = pool, caps
	}
	s.st.Runs[s.worktree] = s.run
	return nil
}

// holder returns the run whose journal has id, preferring this worktree's,
// so a stranded hold can be finished from any worktree of the clone.
func (s *Store) holder(id string) *Run {
	if _, ok := s.run.Held[id]; ok {
		return s.run
	}
	for _, r := range s.st.Runs {
		if _, ok := r.Held[id]; ok {
			return r
		}
	}
	return s.run
}

func (r *Run) put(id string, e Entry) {
	if r.Held == nil {
		r.Held = Held{}
	}
	r.Held[id] = e
}

// Entry returns the journal entry for id from the last state read.
func (s *Store) Entry(id string) (Entry, bool) {
	e, ok := s.holder(id).Held[id]
	return e, ok
}

// Intend records a ref write before it is pushed. A bead this run already
// confirmed stays held, because the old sha is still its claim if the push
// never landed.
func (s *Store) Intend(id, sha string) error {
	return s.update(func() {
		r := s.holder(id)
		e, ok := r.Held[id]
		if !ok {
			e = Entry{Phase: PhasePushing}
		}
		e.Intended = sha
		r.put(id, e)
	})
}

// Confirm records that the remote ref now holds sha.
func (s *Store) Confirm(id, sha string) error {
	return s.update(func() {
		r := s.holder(id)
		e := r.Held[id]
		e.SHA, e.Intended, e.Phase = sha, "", PhaseHeld
		r.put(id, e)
	})
}

// Abandon records that an intended write did not land. An entry that was
// never confirmed goes with it.
func (s *Store) Abandon(id string) error {
	return s.update(func() {
		r := s.holder(id)
		e, ok := r.Held[id]
		if !ok {
			return
		}
		if e.Phase == PhasePushing {
			delete(r.Held, id)
			return
		}
		e.Intended = ""
		r.Held[id] = e
	})
}

// IntendDelete records that this run is deleting its ref. Deleting a ref this
// clone holds no entry for, as sweep does, records nothing.
func (s *Store) IntendDelete(id string) error {
	return s.update(func() {
		r := s.holder(id)
		if e, ok := r.Held[id]; ok {
			e.DeletePending = true
			r.Held[id] = e
		}
	})
}

// Drop forgets id for every run in this clone.
func (s *Store) Drop(id string) error {
	return s.update(func() { s.DropHold(id) })
}

// Run returns the run for this worktree.
func (s *Store) Run() *Run { return s.run }

// Holds lists every bead held by any run in this clone, ordered by bead id.
func (s *Store) Holds() []Hold {
	out := []Hold{}
	for worktree, r := range s.st.Runs {
		_, err := os.Stat(worktree)
		for id, e := range r.Held {
			out = append(out, Hold{ID: id, Run: r.ID, Worktree: worktree, Stranded: errors.Is(err, os.ErrNotExist), Entry: e})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].ID != out[b].ID {
			return out[a].ID < out[b].ID
		}
		return out[a].Worktree < out[b].Worktree
	})
	return out
}

// DropHold forgets id for every run in this clone.
func (s *Store) DropHold(id string) {
	for _, r := range s.st.Runs {
		r.Drop(id)
	}
}

// Save persists the state, removing runs whose worktree is gone and that
// hold nothing. It requires a Store opened with Write.
func (s *Store) Save() error {
	if !s.locked {
		return errors.New("runstate: Save needs a Store opened with Write")
	}
	for worktree, r := range s.st.Runs {
		if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) && len(r.Held) == 0 {
			delete(s.st.Runs, worktree)
		}
	}
	return s.write()
}

// Hold records that this run holds id, keeping any journal entry it has.
func (r *Run) Hold(id string) {
	if _, ok := r.Held[id]; !ok {
		r.put(id, Entry{Phase: PhaseHeld})
	}
}

// Drop forgets id.
func (r *Run) Drop(id string) { delete(r.Held, id) }

// HeldIDs lists the beads this run holds, in order.
func (r *Run) HeldIDs() []string {
	ids := make([]string, 0, len(r.Held))
	for id := range r.Held {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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
