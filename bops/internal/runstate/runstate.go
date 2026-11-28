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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Run is the persisted state for one worktree of a clone, or for one named
// run in a worktree (BOPS_RUN).
type Run struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"` // BOPS_RUN; empty for the worktree's own run
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
	// ReleasePending is a release written to the local ledger whose push has
	// not landed: the ref stays until it has, so no other machine sees the
	// ref gone while the remote ledger still shows a claim.
	ReleasePending bool `json:"release_pending,omitempty"`
}

// Given reports whether the hold was released and waits only on its ledger
// push or ref delete: it is no claim of this run's any more.
func (e Entry) Given() bool { return e.DeletePending || e.ReleasePending }

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
//
// Version 3 adds no field: it marks a clone whose bops processes serialize
// each bead (LockBead), so an older bops, which does not, refuses the file
// and asks to be upgraded rather than race the newer ones (bops-yph).
//
// Version 4 adds no field either: it marks a clone whose bead lock files may
// be pruned, which is safe only for a bops that re-checks after locking that
// its lock file was not replaced (bops-exb). bops 0.4.0 to 0.4.3 does not, so
// it refuses a version 4 file, and a prune runs only on a file already
// stamped.
//
// Version 5 adds named runs (BOPS_RUN): a run keyed by its worktree root and
// "#run=<name>", with the name also in the run's name field (bops-4ez). A
// bops before it would read that key as a removed worktree and offer the
// run's live holds as stranded, so it refuses the file instead.
//
// Version 6 adds release_pending to a hold: a release written to the local
// ledger whose push has not landed (reap's). A bops before it would read
// such a hold as a live claim, show it again and heartbeat it, and drop the
// flag on its next write, so it refuses the file instead.
const stateVersion = 6

// Mode says whether a Store may be saved.
type Mode int

const (
	// Read sees the current state and takes the lock only to record a new run
	// or to stamp an older file with this version, which it skips if it cannot.
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
	Runs    map[string]*Run `json:"runs"` // keyed by runKey
}

// runSep joins a worktree root and a run name in a state key. A worktree's
// own run is keyed by its root alone, as before named runs existed.
const runSep = "#run="

// runKey is the state key of the run name in worktree; "" is its own run.
func runKey(worktree, name string) string {
	if name == "" {
		return worktree
	}
	return worktree + runSep + name
}

// keyWorktree is the worktree root a state key belongs to.
func keyWorktree(key string) string {
	if i := strings.LastIndex(key, runSep); i >= 0 {
		return key[:i]
	}
	return key
}

// runNamePattern is what BOPS_RUN may be: short, and plain enough to sit in
// a state key and a message.
var runNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// RunName reads BOPS_RUN, the name of a run of its own within a worktree.
// An orchestrator that claims from a checkout people also work in sets it,
// so its holds are not the checkout's run and a session started there
// neither re-displays nor heartbeats them (bops-4ez). Unset, a command is
// the worktree's own run.
func RunName() (string, error) {
	n := os.Getenv("BOPS_RUN")
	if n == "" || runNamePattern.MatchString(n) {
		return n, nil
	}
	return "", fmt.Errorf("BOPS_RUN %q is not a run name: use up to 64 letters, digits, '.', '_' or '-', starting with a letter or digit", n)
}

// Store is one process's view of a clone's state file.
type Store struct {
	dir      string
	worktree string
	name     string // BOPS_RUN, or "" for the worktree's own run
	key      string // the run's key in state.Runs (runKey)
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
	name, err := RunName()
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, worktree: worktree, name: name, key: runKey(worktree, name)}
	if mode == Write {
		if err := s.lock(); err != nil {
			return nil, err
		}
	}
	if err := s.ensureRun(); err != nil {
		s.Close()
		return nil, err
	}
	s.run = s.st.Runs[s.key]
	// Stamp an older file at once, not at this process's first write: from
	// then on an older bops, which takes no bead lock, refuses the clone
	// rather than run beside this one unserialized (bops-j9k). It comes
	// before the actor override, which is this process's alone. A read-only
	// command that cannot stamp still reads; a later command stamps.
	if s.st.Version < stateVersion {
		if err := s.stamp(mode); err != nil {
			s.Close()
			return nil, err
		}
	}
	if a := os.Getenv("BEADS_ACTOR"); a != "" {
		s.run.Actor = a // the environment names who is acting now, not who created the run
	} else if s.run.Actor == "" {
		s.run.Actor = Actor() // a run recorded without one; other runs' are left as stored
	}
	if mode == Read {
		s.Close()
	}
	return s, nil
}

// stamp writes an older file back at this version. In Write mode, which
// holds the lock and read the file under it, a failure is an error. A
// read-only command stamps only if it can take the lock at once and write,
// and otherwise leaves the file for a later command; it re-reads the file
// under the lock and writes only that, so nothing it read earlier is written
// back. A file that cannot be re-read (a newer version, say) is an error.
func (s *Store) stamp(mode Mode) error {
	if mode == Write {
		return s.write()
	}
	if !s.locked { // ensureRun may have left it held
		if s.lockWithin(0) != nil {
			return nil
		}
	}
	defer s.Close()
	if err := s.read(); err != nil {
		return err
	}
	// read gives an empty state for a file that is gone, so a file removed
	// since Open read it is not recreated; one left with no runs is likewise
	// left for a later write to stamp.
	if s.st.Version < stateVersion && len(s.st.Runs) > 0 {
		_ = s.write()
	}
	if fresh := s.st.Runs[s.key]; fresh != nil {
		s.run = fresh
	} else {
		s.st.Runs[s.key] = s.run
	}
	return nil
}

func (s *Store) ensureRun() error {
	if err := s.read(); err != nil {
		return err
	}
	if s.st.Runs[s.key] != nil {
		return nil
	}
	if !s.locked {
		if err := s.lock(); err != nil {
			return err
		}
		if err := s.read(); err != nil { // another process may have added it
			return err
		}
		if s.st.Runs[s.key] != nil {
			return nil
		}
	}
	s.st.Runs[s.key] = &Run{ID: NewID(), Name: s.name, Machine: hostname(), Actor: Actor(), StartedAt: time.Now().UTC()}
	return s.write()
}

func (s *Store) read() error {
	st, err := s.load()
	s.st = st
	return err
}

// load reads the state file without touching the Store's view of it. A
// missing file is an empty state.
func (s *Store) load() (state, error) {
	st := state{Runs: map[string]*Run{}}
	b, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("state file %s: %w", filepath.Join(s.dir, "state.json"), err)
	}
	if st.Version > stateVersion {
		return st, fmt.Errorf("state file %s is version %d; this bops understands up to %d, so upgrade it", filepath.Join(s.dir, "state.json"), st.Version, stateVersion)
	}
	if st.Runs == nil {
		st.Runs = map[string]*Run{}
	}
	return st, nil
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
	// On Windows a rename onto a file another process has open for reading
	// fails until it closes it, which takes milliseconds; retry briefly.
	var err2 error
	for try := 0; try < renameTries; try++ {
		if err2 = os.Rename(tmp, filepath.Join(s.dir, "state.json")); err2 == nil || runtime.GOOS != "windows" {
			return err2
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err2
}

const renameTries = 10

// lock takes the clone's write lock. A lock left by a crashed process is
// broken after lockStale. Two waiters that both break the same stale lock
// can, in a very narrow window, both proceed.
func (s *Store) lock() error { return s.lockWithin(lockWait) }

// lockWithin is lock waiting at most wait; with 0 it tries once.
func (s *Store) lockWithin(wait time.Duration) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "lock")
	deadline := time.Now().Add(wait)
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

// Refresh re-reads the state file, keeping the run this Store hands out.
// A command calls it once it holds a bead's lock (LockBead), so what it then
// reads of that bead is current and stays so until it lets go.
func (s *Store) Refresh() error { return s.refresh() }

// refresh re-reads the file while keeping s.run the same pointer, because
// callers hold it. Pool, caps and actor are per-process overrides and stay.
func (s *Store) refresh() error {
	if err := s.read(); err != nil {
		return err
	}
	if s.run == nil {
		return nil
	}
	if fresh := s.st.Runs[s.key]; fresh != nil {
		pool, caps, actor := s.run.Pool, s.run.Caps, s.run.Actor
		*s.run = *fresh
		s.run.Pool, s.run.Caps, s.run.Actor = pool, caps, actor
	}
	s.st.Runs[s.key] = s.run
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
		// A confirmed write supersedes a delete that never finished: the ref
		// is this claim now, and sync must not go on to delete it.
		e.SHA, e.Intended, e.Phase, e.DeletePending, e.ReleasePending = sha, "", PhaseHeld, false, false
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

// SetLedgerPending records whether the ledger display still needs writing.
func (s *Store) SetLedgerPending(id string, pending bool) error {
	return s.update(func() {
		r := s.holder(id)
		if e, ok := r.Held[id]; ok {
			e.LedgerPending = pending
			r.Held[id] = e
		}
	})
}

// SetReleasePending records whether a release of id waits on its ledger push.
// Marking a bead no run here holds is an error: nothing would finish it.
func (s *Store) SetReleasePending(id string, pending bool) error {
	found := false
	err := s.update(func() {
		r := s.holder(id)
		if e, ok := r.Held[id]; ok {
			found = true
			e.ReleasePending = pending
			r.Held[id] = e
		}
	})
	if err == nil && !found && pending {
		err = fmt.Errorf("no hold of %s to mark release pending", id)
	}
	return err
}

// Drop forgets id for every run in this clone.
func (s *Store) Drop(id string) error {
	return s.update(func() { s.DropHold(id) })
}

// Matches reports whether run's journal, as this Store last read the file,
// has expect for id. It reads and writes nothing: the caller has just taken
// the bead's lock and re-read the file (Refresh), so what it holds for that
// bead is current and stays so.
func (s *Store) Matches(run, id string, expect Entry) bool {
	for _, r := range s.st.Runs {
		if r.ID == run {
			cur, ok := r.Held[id]
			return ok && cur == expect
		}
	}
	return false
}

// ErrBeadBusy is LockBead's answer when another process holds the bead.
var ErrBeadBusy = errors.New("another bops process on this machine is working on this bead")

// ErrBeadLockUnsupported is LockBead's answer when the state directory's file
// system has no file locks (some network and FUSE mounts).
var ErrBeadLockUnsupported = errors.New("the state directory's file system does not support file locks")

// LockBead takes this clone's lock on bead id for as long as the caller works
// on it, network calls included, so two bops processes sharing the clone
// (two worktrees, or two sessions in one checkout, which share its run) never
// write one bead's claim at once (bops-yph). It never waits: a held lock is
// ErrBeadBusy. The operating system releases it when the process exits, so a
// crash leaves nothing to clean up. Call the returned function to release it.
//
// The file is named by a digest of the id, so every name is short and plain:
// no id is a device name on Windows (nul, con), too long for the file
// system, or shares a file with another that differs only in case.
func (s *Store) LockBead(id string) (func(), error) {
	if id == "" {
		return nil, errors.New("an empty bead id cannot name a lock file")
	}
	path := beadLockPath(s.dir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return lockFile(path)
}

// beadLockPath is the lock file for bead id in the state directory dir.
func beadLockPath(dir, id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(dir, "beads", hex.EncodeToString(sum[:16])+".lock")
}

// beadLockName matches the names beadLockPath gives, so a prune touches
// nothing else in the directory.
var beadLockName = regexp.MustCompile(`^[0-9a-f]{32}\.lock$`)

// PruneBeadLocks removes the lock files of beads no run in this clone holds,
// which would otherwise pile up, one per bead ever touched (bops-exb). It
// never waits: a file another process has locked is left for a later prune.
// It removes a file only while holding its lock, and LockBead re-checks after
// locking that its path still names the file it locked, so no two processes
// of this version hold one bead's lock on two files. An older bops does not
// re-check, so a prune runs only once the state file on disk carries this
// version, which that bops refuses; a read-only command's stamp can be
// skipped, so the file itself is checked. It reads the file afresh and leaves
// the Store's view alone. On Windows it removes nothing (see pruneLockFile
// there). It returns how many files it removed, and the first error any file
// gave.
func (s *Store) PruneBeadLocks() (int, error) {
	st, err := s.load()
	if err != nil {
		return 0, err
	}
	if st.Version != stateVersion {
		return 0, nil
	}
	keep := map[string]bool{}
	for _, r := range st.Runs {
		for id := range r.Held {
			keep[filepath.Base(beadLockPath(s.dir, id))] = true
		}
	}
	for id := range s.run.Held { // this process's own, should they be newer
		keep[filepath.Base(beadLockPath(s.dir, id))] = true
	}
	dir := filepath.Join(s.dir, "beads")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, e := range entries {
		if keep[e.Name()] || !beadLockName.MatchString(e.Name()) {
			continue
		}
		removed, err := pruneLockFile(filepath.Join(dir, e.Name()))
		if removed {
			n++
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return n, first
}

// Event is one line of events.jsonl, the local record of what bops did (D21).
type Event struct {
	At      time.Time `json:"at"`
	Run     string    `json:"run"`
	Command string    `json:"command"`
	Bead    string    `json:"bead"`
	Step    string    `json:"step"`
	Outcome string    `json:"outcome"`
	Code    string    `json:"code,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	// WaitedMS and HeldMS time a command's bead lock (step bead_lock): how
	// long it waited for the lock and how long it then held it (bops-7uu).
	// They are set only on those lines, where 0 is a real value.
	WaitedMS *int64 `json:"waited_ms,omitempty"`
	HeldMS   *int64 `json:"held_ms,omitempty"`
	// Tries is how many push tries a ledger_push line's push made (each
	// after the first pulls first); 0, and omitted, when it made none.
	Tries int `json:"tries,omitempty"`
}

// eventsMax is the size at which events.jsonl is rotated to one old copy.
const eventsMax = 1 << 20

// rotate moves a full events.jsonl to events.jsonl.1. Two processes that
// both found it full would otherwise both rename it, the second moving the
// first's fresh file over the old copy and losing it, so it rotates under its
// own operating-system lock and re-checks the size there (bops-qef). It never
// waits: a process that holds the lock is rotating, and Log appends to
// whichever file it finds. A lock it cannot take for any other reason (a file
// system with no locks, an events.lock it cannot open) leaves it rotating
// unserialized, as before, rather than letting the file grow without end.
func (s *Store) rotate(path string) {
	beforeRotate()
	release, err := lockFile(filepath.Join(s.dir, "events.lock"))
	switch {
	case errors.Is(err, ErrBeadBusy):
		return
	case err != nil: // no lock to be had: rotate unserialized, as before
	default:
		defer release()
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= eventsMax {
		_ = os.Rename(path, path+".1")
	}
}

// beforeRotate runs between Log finding events.jsonl full and rotate taking
// its lock. Tests set it to rotate from another Log in that window; a test
// that does must not run in parallel.
var beforeRotate = func() {}

// Log appends an event. Losing one must never fail the command it describes.
func (s *Store) Log(ev Event) {
	path := filepath.Join(s.dir, "events.jsonl")
	if info, err := os.Stat(path); err == nil && info.Size() >= eventsMax {
		s.rotate(path)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Run returns the run this Store acts as: the worktree's own, or BOPS_RUN's.
func (s *Store) Run() *Run { return s.run }

// Worktree is the root of the worktree this Store's run belongs to.
func (s *Store) Worktree() string { return s.worktree }

// Holds lists every bead held by any run in this clone, ordered by bead id.
func (s *Store) Holds() []Hold {
	out := []Hold{}
	for key, r := range s.st.Runs {
		worktree := keyWorktree(key)
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
	for key, r := range s.st.Runs {
		if _, err := os.Stat(keyWorktree(key)); errors.Is(err, os.ErrNotExist) && len(r.Held) == 0 {
			delete(s.st.Runs, key)
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
