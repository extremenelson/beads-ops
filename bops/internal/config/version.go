package config

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// keyMinBops is min_bops' path, shared by its row in keys and the guard that
// accepts it only from the repo file.
const keyMinBops = "min_bops"

// minVersion is what min_bops accepts: a release, MAJOR.MINOR.PATCH, with an
// optional leading v. A prerelease cannot be a floor every machine meets.
var minVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// runningVersion is a version a binary can report: a release, or a
// prerelease or Go pseudo-version (v0.5.2-0.20261120000000-abcdef123456),
// with optional +build metadata such as +dirty.
var runningVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// minBopsHere refuses min_bops from a layer above the repo file. The floor is
// the team's, in the committed file every machine reads; a user file or the
// environment could otherwise lower it for one machine (bops-n4r).
func minBopsHere(c *Config, repoMin string) error {
	if c.MinBops != repoMin {
		return errMinBopsLayer
	}
	return nil
}

var errMinBopsLayer = errors.New("min_bops is accepted only from " + RepoFile + ", the committed file every machine reads; another layer could lower it for one machine")

// BelowMin compares a running bops version with min_bops. known is false when
// running is not a version at all ("dev", a build with no version stamped),
// so the floor cannot be checked. A prerelease or pseudo-version sorts before
// its release, as in semver: v0.5.2-0.2026… may predate v0.5.2.
func BelowMin(running, min string) (below, known bool) {
	r := runningVersion.FindStringSubmatch(running)
	m := minVersion.FindStringSubmatch(min)
	if r == nil || m == nil {
		return false, false
	}
	for i := 1; i <= 3; i++ {
		a, _ := strconv.Atoi(r[i])
		b, _ := strconv.Atoi(m[i])
		if a != b {
			return a < b, true
		}
	}
	return r[4] != "", true // same release: a prerelease of it is below it
}

// normalizeMin drops min_bops' optional leading v, so messages and the
// install command read the same however it was written.
func normalizeMin(v string) string { return strings.TrimPrefix(v, "v") }
