package runstate

import "time"

// pullFailFile records a claim's ledger pull that failed. Like the conflict
// record it sits beside state.json, so state.json keeps its version.
const pullFailFile = "ledger-pull-failed.json"

// LedgerPullFailure is the last ledger pull on a claim path (claim,
// claim --force, claim --resume) in this clone that failed for any reason
// other than a merge conflict needing a person (that has its own record).
// The claim went on without the pull and without re-checking the bead
// after it, and under lock.mode ref it does not push, so nothing else would
// show the failure: status and doctor report it. The next pull that
// succeeds removes it.
type LedgerPullFailure struct {
	At      time.Time `json:"at"`
	Command string    `json:"command,omitempty"`
	Run     string    `json:"run,omitempty"`
	Bead    string    `json:"bead,omitempty"`
	Detail  string    `json:"detail"`
}

// SetLedgerPullFailure records f, or with nil removes the record, as
// SetLedgerConflict does.
func (s *Store) SetLedgerPullFailure(f *LedgerPullFailure) error {
	if f == nil {
		return s.writeRecord(pullFailFile, nil)
	}
	return s.writeRecord(pullFailFile, f)
}

// LedgerPullFailure reads the record, or nil when there is none.
func (s *Store) LedgerPullFailure() (*LedgerPullFailure, error) {
	return readPullFailure(s.dir)
}

// ReadLedgerPullFailure reads the record of the clone holding start without
// opening its state (doctor).
func ReadLedgerPullFailure(start string) (*LedgerPullFailure, error) {
	dir, err := clonesStateDir(start)
	if err != nil {
		return nil, err
	}
	return readPullFailure(dir)
}

func readPullFailure(dir string) (*LedgerPullFailure, error) {
	f := &LedgerPullFailure{}
	if ok, err := readRecord(dir, pullFailFile, f); !ok || err != nil {
		return nil, err
	}
	return f, nil
}
