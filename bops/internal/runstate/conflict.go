package runstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// conflictFile records a ledger pull that stopped for a person. It sits
// beside state.json rather than in it, so state.json keeps its version and an
// older bops neither refuses the clone nor drops the record when it writes.
const conflictFile = "ledger-conflict.json"

// LedgerConflict is the last ledger pull in this clone that stopped on a
// merge conflict bd will not settle itself. Until a person resolves it, every
// later pull and push from the clone fails the same way, so the record
// outlives the command that met it: status and doctor report it. The next
// pull that succeeds removes it.
type LedgerConflict struct {
	At      time.Time `json:"at"`
	Command string    `json:"command,omitempty"`
	Run     string    `json:"run,omitempty"`
	// Ref is the remote branch the pull merged, as bd named it.
	Ref string `json:"ref,omitempty"`
	// Beads are the beads this clone has that Ref does not, and RemoteOnly
	// the beads Ref has that this clone does not, when BeadsRead. The bead
	// in conflict is in one of them.
	Beads      []string `json:"beads"`
	RemoteOnly []string `json:"remote_only"`
	BeadsRead  bool     `json:"beads_read"`
	Detail     string   `json:"detail"`
}

// SetLedgerConflict records c, or with nil removes the record. It replaces
// the file atomically and takes no lock. Two processes sharing the clone can
// still write out of order (a record landing after a later pull's clear, say);
// such a record is wrong only until the next pull, which records or clears it
// again.
func (s *Store) SetLedgerConflict(c *LedgerConflict) error {
	path := filepath.Join(s.dir, conflictFile)
	if c == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, conflictFile+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// LedgerConflict reads the record, or nil when there is none.
func (s *Store) LedgerConflict() (*LedgerConflict, error) {
	return readConflict(s.dir)
}

// ReadLedgerConflict reads the record of the clone holding start without
// opening its state, so it writes nothing and creates no run (doctor).
func ReadLedgerConflict(start string) (*LedgerConflict, error) {
	commonDir, _, err := locate(start)
	if err != nil {
		return nil, err
	}
	dir, err := stateDir(commonDir)
	if err != nil {
		return nil, err
	}
	return readConflict(dir)
}

func readConflict(dir string) (*LedgerConflict, error) {
	path := filepath.Join(dir, conflictFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := &LedgerConflict{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
