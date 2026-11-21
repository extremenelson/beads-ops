package main

import (
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/extremenelson/beads-ops/bops/internal/config"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// gateWarnings are added to this command's document by minBopsGate: a read
// run by a bops older than min_bops still answers, with a warning. run resets
// it for each command.
var gateWarnings []string

// runningVersion is this binary's version as `bops version` reports it.
// Tests replace it.
var runningVersion = func() string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, info)
}

// installHint is the upgrade fix every min_bops message names.
func installHint(min string) string {
	return fmt.Sprintf("upgrade it: go install %s/cmd/bops@v%s (or later)", bopsModule, min)
}

// minBopsGate applies min_bops in .bops.json before a command runs
// (bops-n4r). A write, or a read+sync such as session-start, by a bops older
// than the floor is refused with bops_too_old: an old binary may not honour
// the lock as the ledger expects. A read answers with a warning. A bops with
// no version ("dev") cannot be compared, so it runs; doctor says so. A config
// that does not load is left to the command, which reports bad_config.
func minBopsGate(kind string) (warn string, err error) {
	cfg, err := loadConfig()
	if err != nil || cfg.MinBops == "" {
		return "", nil
	}
	running := runningVersion()
	below, known := config.BelowMin(running, cfg.MinBops)
	if !known || !below {
		return "", nil
	}
	msg := fmt.Sprintf("this bops is %s, older than min_bops %s in %s; %s", running, cfg.MinBops, config.RepoFile, installHint(cfg.MinBops))
	if kind == "read" {
		return msg + "; until then this read may not match what the ledger expects, and every write is refused", nil
	}
	return "", &lock.Error{Code: lock.CodeTooOld, Err: errors.New(msg)}
}

// minBopsCheck is doctor's bops_version check: a failed check when this bops
// is older than min_bops, a passing one when it meets it, and only a warning
// when it has no version to compare. With no min_bops it adds nothing.
func minBopsCheck(min, running string) (*lock.Check, string) {
	if min == "" {
		return nil, ""
	}
	below, known := config.BelowMin(running, min)
	switch {
	case !known:
		return nil, fmt.Sprintf("min_bops %s is set, but this bops is %q, a build with no version, so it cannot be checked; a release build reports its version", min, running)
	case below:
		return &lock.Check{ID: "bops_version", Status: lock.CheckFail, Code: lock.CodeTooOld,
			Detail: fmt.Sprintf("this bops is %s, older than min_bops %s in %s: every write is refused", running, min, config.RepoFile),
			Next:   installHint(min)}, ""
	}
	return &lock.Check{ID: "bops_version", Status: lock.CheckOK, Detail: fmt.Sprintf("this bops is %s, at least min_bops %s", running, min)}, ""
}
