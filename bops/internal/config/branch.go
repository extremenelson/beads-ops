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
// skill never shows a command that cannot work.
func validBranchPattern(v string) bool {
	return len(v) <= maxBranchPattern && branchPattern.MatchString(v) &&
		!strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "/") && !strings.HasSuffix(v, "/") &&
		!strings.HasSuffix(v, ".") && !strings.HasSuffix(v, ".lock") &&
		!strings.Contains(v, "..") && !strings.Contains(v, "//")
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
