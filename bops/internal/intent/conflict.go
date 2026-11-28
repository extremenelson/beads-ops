package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// A ledger pull can stop on a merge conflict bd will not settle itself. On
// bd 1.3.4 a bead deleted on one machine while another changed its row (a
// claim display, a note, land's landed record) does this, and bd prints
//
//	Error: merge origin/main: merge conflicts in issues require operator resolution; merge aborted and working set restored
//
// (testdata/bd-1.3.4-pull-operator-resolution.stderr). The merge is aborted,
// so nothing is left in Dolt's conflict tables, and every later pull and push
// from the clone fails the same way until a person resolves it: a retry
// cannot succeed. The first pull that meets it records it in the clone's
// state directory (status and doctor report it), logs a ledger_pull event
// with outcome conflict, and every later pull and push in the same command
// returns the same error without calling bd.

// ledgerConflictMarker is the stable part of bd's message: the table name
// and the remote branch before it vary.
const ledgerConflictMarker = "require operator resolution"

// conflictRef finds the remote branch bd says it was merging. Only a
// branch-like name is taken (bd 1.3.4 names the tracking branch, such as
// origin/main): it goes to bd diff as an argument and into the record.
var conflictRef = regexp.MustCompile(`merge ([A-Za-z0-9_.][A-Za-z0-9_./-]*): merge conflicts in `)

// diffWait bounds the bd diff that names the beads. It runs apart from the
// pull's deadline, which session-start keeps short.
const diffWait = 15 * time.Second

// LedgerConflictRunbook is the next step for a stuck ledger: MULTI-DEV-PLAN
// §6's exit-2 runbook, with what each strategy did on bd 1.3.4
// (docs/research/2026-11-ledger-contention.md, Same-row pulls).
const LedgerConflictRunbook = "every pull and push from this clone fails until a person resolves it here (docs/MULTI-DEV-PLAN.md §6, exit-2 runbook): " +
	"ask whoever deleted the bead whether the delete stands; `bd dolt pull --strategy theirs` keeps the remote's side (a bead deleted there stays deleted; on bd 1.3.4 it can instead fail with a foreign key error), " +
	"`bd dolt pull --strategy ours` keeps this clone's (a bead deleted there comes back without its labels and comments; then `bd delete <id> --force` if the delete stands); " +
	"either strategy leaves tables uncommitted, so run `bd dolt commit`, then `bops sync`"

// isLedgerConflict reports a pull that stopped for a person, from bd's
// message or from an error doltPull already marked.
func isLedgerConflict(err error) bool {
	return err != nil && (lock.CodeOf(err) == lock.CodeLedgerConflict || strings.Contains(err.Error(), ledgerConflictMarker))
}

// doltPull runs bd dolt pull. A pull that succeeds clears a recorded
// conflict; one that stops for a person is recorded and returned with the
// code ledger_conflict, and so is every later pull in this command.
func (e *Env) doltPull(ctx context.Context) ([]byte, error) {
	if e.ledgerConflict != nil {
		return nil, e.ledgerConflict
	}
	start := e.now()
	b, err := withLockRetry(ctx, e, "dolt", "pull")
	switch {
	case err == nil:
		e.clearLedgerConflict(start)
		e.clearPullFailure(ctx, start)
	case isLedgerConflict(err):
		e.ledgerConflict = e.recordLedgerConflict(ctx, start, err)
		err = e.ledgerConflict
	}
	return b, err
}

// recordLedgerConflict reads which beads differ between the clone and the
// remote branch, records the conflict as of the pull's start and logs it, and
// returns the error a command reports.
func (e *Env) recordLedgerConflict(ctx context.Context, start time.Time, pullErr error) error {
	c := runstate.LedgerConflict{At: start, Command: e.Command, Detail: lock.Scrub(pullErr.Error()), Beads: []string{}, RemoteOnly: []string{}}
	if e.Run != nil {
		c.Run = e.Run.ID
	}
	if m := conflictRef.FindStringSubmatch(c.Detail); m != nil {
		c.Ref = m[1]
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), diffWait)
		if here, there, err := e.diffAgainst(dctx, c.Ref); err == nil {
			c.Beads, c.RemoteOnly, c.BeadsRead = here, there, true
		}
		cancel()
	}
	msg := DescribeLedgerConflict(&c)
	if e.Store != nil {
		if err := e.Store.SetLedgerConflict(&c); err != nil {
			msg += "; not recorded for status and doctor: " + lock.Scrub(err.Error())
		}
		ev := runstate.Event{At: start, Command: e.Command, Step: "ledger_pull", Outcome: "conflict", Code: lock.CodeLedgerConflict, Detail: msg}
		if e.Run != nil {
			ev.Run = e.Run.ID
		}
		e.Store.Log(ev)
	}
	return &lock.Error{Code: lock.CodeLedgerConflict, Err: fmt.Errorf("%s; next: %s", msg, LedgerConflictRunbook)}
}

// diffAgainst lists, from `bd diff HEAD <ref> --json`, the beads at HEAD
// that ref does not have (here: deleted there, or created here and not yet
// pushed) and the beads ref has that HEAD does not (there: deleted here, or
// created there). It reads the clone's copy of the remote branch the failed
// pull fetched (no network). The aborted merge leaves no conflict table to
// read, so this is the nearest bd gives: the conflicting bead is in one list.
func (e *Env) diffAgainst(ctx context.Context, ref string) (here, there []string, err error) {
	raw, err := e.BD.Run(ctx, "diff", "HEAD", ref, "--json")
	if err != nil {
		return nil, nil, err
	}
	var rows []struct {
		IssueID  string `json:"IssueID"`
		DiffType string `json:"DiffType"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, err
	}
	here, there = []string{}, []string{}
	for _, r := range rows {
		switch {
		case r.IssueID == "":
		case r.DiffType == "removed":
			here = append(here, r.IssueID)
		case r.DiffType == "added":
			there = append(there, r.IssueID)
		}
	}
	sort.Strings(here)
	sort.Strings(there)
	return here, there, nil
}

// clearLedgerConflict removes a recorded conflict once a pull that started
// at start succeeds. A record of a pull that started later is kept: another
// process sharing the clone met the conflict after this pull began. A record
// that cannot be read is removed, so it cannot outlive a pull that succeeded.
func (e *Env) clearLedgerConflict(start time.Time) {
	if e.Store == nil {
		return
	}
	if c, err := e.Store.LedgerConflict(); err == nil && (c == nil || c.At.After(start)) {
		return
	}
	if err := e.Store.SetLedgerConflict(nil); err != nil {
		return // the next pull that succeeds tries again
	}
	ev := runstate.Event{At: e.now(), Command: e.Command, Step: "ledger_pull", Outcome: "resolved"}
	if e.Run != nil {
		ev.Run = e.Run.ID
	}
	e.Store.Log(ev)
}

// DescribeLedgerConflict says what stopped and which beads it names.
func DescribeLedgerConflict(c *runstate.LedgerConflict) string {
	by := "a ledger pull"
	if c.Command != "" {
		by = "bops " + c.Command + "'s ledger pull"
	}
	s := fmt.Sprintf("%s at %s stopped on a merge conflict bd leaves for a person (%s)", by, c.At.UTC().Format(time.RFC3339), c.Detail)
	ref := c.Ref
	if ref == "" {
		ref = "the remote"
	}
	if !c.BeadsRead {
		return s + "; which beads conflict could not be read"
	}
	if len(c.Beads) == 0 && len(c.RemoteOnly) == 0 {
		return s + "; bd lists no bead that only one of this clone and " + ref + " has"
	}
	if len(c.Beads) > 0 {
		s += "; beads this clone has that " + ref + " does not (deleted there, or created here and not yet pushed): " + strings.Join(c.Beads, ", ")
	}
	if len(c.RemoteOnly) > 0 {
		s += "; beads " + ref + " has that this clone does not (deleted here, or created there): " + strings.Join(c.RemoteOnly, ", ")
	}
	return s
}

// LedgerConflictReport is a recorded conflict with its next step, for status.
type LedgerConflictReport struct {
	runstate.LedgerConflict
	Next string `json:"next"`
}

// ledgerConflictReport is the clone's recorded conflict, or nil.
func (e *Env) ledgerConflictReport(warnings *[]string) *LedgerConflictReport {
	if e.Store == nil {
		return nil
	}
	c, err := e.Store.LedgerConflict()
	if err != nil {
		*warnings = append(*warnings, "ledger conflict record not read: "+lock.Scrub(err.Error()))
		return nil
	}
	if c == nil {
		return nil
	}
	*warnings = append(*warnings, "ledger stuck: "+DescribeLedgerConflict(c)+"; next: "+LedgerConflictRunbook)
	return &LedgerConflictReport{LedgerConflict: *c, Next: LedgerConflictRunbook}
}

// LedgerConflictCheckID is doctor's check for a recorded conflict.
const LedgerConflictCheckID = "ledger_conflict"

// LedgerConflictCheck is doctor's ledger_conflict check: a failed check when
// the clone holding dir has a recorded conflict. It only reads the record.
// ok is false when there is none; a record that cannot be read is a warning.
func LedgerConflictCheck(dir string) (c lock.Check, ok bool, warning string) {
	rec, err := runstate.ReadLedgerConflict(dir)
	if err != nil {
		return lock.Check{}, false, "ledger conflict record not read: " + lock.Scrub(err.Error())
	}
	if rec == nil {
		return lock.Check{}, false, ""
	}
	return lock.Check{ID: LedgerConflictCheckID, Status: lock.CheckFail, Code: lock.CodeLedgerConflict,
		Detail: lock.Scrub(DescribeLedgerConflict(rec)), Next: LedgerConflictRunbook}, true, ""
}
