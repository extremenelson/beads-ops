package config

import (
	"errors"
	"regexp"
	"strings"
)

// keyBranchPattern is branch_pattern's path, shared by its row in keys and
// the guard that accepts it only from the repo file.
const keyBranchPattern = "branch_pattern"

// DefaultBranchPattern is the branch the generated skill names when the repo
// file sets no branch_pattern.
const DefaultBranchPattern = "work/<id>"

// maxBranchPattern keeps the generated skill under its token cap whatever
// pattern a repo sets (bops-onn).
const maxBranchPattern = 40

// branchPattern is what branch_pattern accepts: letters, digits, '.', '_',
// '/' and '-', and <placeholder> words such as <id> or <slug>, so it can sit
// in the skill's commands unquoted.
var branchPattern = regexp.MustCompile(`^([A-Za-z0-9._/-]|<[a-z][a-z0-9_-]*>)+$`)

// validBranchPattern also refuses what git refuses in a branch name, so the
// skill never shows a command that cannot work, and wants <id> as a bounded
// token (carriesID).
func validBranchPattern(v string) bool {
	return len(v) <= maxBranchPattern && branchPattern.MatchString(v) &&
		!strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "/") && !strings.HasSuffix(v, "/") &&
		!strings.HasSuffix(v, ".") && !strings.HasSuffix(v, ".lock") && !strings.Contains(v, ".lock/") &&
		!strings.HasPrefix(v, ".") && !strings.Contains(v, "/.") &&
		!strings.Contains(v, "..") && !strings.Contains(v, "//") && carriesID(v)
}

// carriesID reports whether pattern has <id> bounded on each side by an end
// or one of IDBounds. reap finds a bead's pushed work by its id as such a
// token in a branch name, so a branch the pattern names without one
// (feat/<slug>, or <slug><id>) would be work it cannot see, and it would
// release the claim as holding none.
func carriesID(pattern string) bool {
	const id = "<id>"
	for from := 0; ; {
		k := strings.Index(pattern[from:], id)
		if k < 0 {
			return false
		}
		k += from
		if Bounded(pattern, k, k+len(id)) {
			return true
		}
		from = k + 1
	}
}

// IDBounds are the characters that bound a bead id in a branch or directory
// name, besides its ends.
const IDBounds = "/-_."

// Bounded reports whether s[k:end] is bounded on each side by an end of s or
// one of IDBounds. It wants 0 <= k <= end <= len(s), as strings.Index gives.
func Bounded(s string, k, end int) bool {
	return (k == 0 || strings.IndexByte(IDBounds, s[k-1]) >= 0) &&
		(end == len(s) || strings.IndexByte(IDBounds, s[end]) >= 0)
}

// branchHere refuses branch_pattern from a layer above the repo file. It
// shapes the skill a repo commits, so one machine's setting would make that
// machine's regenerated skill differ from everyone else's (bops-onn).
func branchHere(c *Config, repoBranch string) error {
	if c.BranchPattern != repoBranch {
		return errBranchLayer
	}
	return nil
}

var errBranchLayer = errors.New("branch_pattern is accepted only from " + RepoFile + ", the committed file the generated skill is built from")
