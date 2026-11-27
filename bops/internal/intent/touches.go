package intent

import (
	"fmt"
	"path"
	"strings"

	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// TouchesPrefix is the label a bead uses to declare a path it expects to
// change: touches:<repo-relative path>. It is optional and advisory: only
// `next --avoid-conflicts` acts on it, and a bead without one never conflicts.
const TouchesPrefix = "touches:"

// touches is the paths a bead declares, as written, without empty ones.
func touches(i *ledger.Issue) []string {
	var out []string
	for _, p := range i.LabelsWithPrefix(TouchesPrefix) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// touchRoot is the directory or file a declared path covers. A path is
// repo-relative with / separators, compared case-sensitively; a leading ./ or
// / and a trailing / are dropped. A glob covers the directory above its first
// segment holding * ? [ or {, so bops/*.go covers bops and * covers the whole
// repo ("."), as does a path that climbs out of the repo with "..". Widening
// only over-reports. Paths are otherwise taken as written, so two spellings
// of one file (another case, an absolute path) do not overlap.
func touchRoot(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	var keep []string
	for _, seg := range strings.Split(p, "/") {
		if strings.ContainsAny(seg, "*?[{") {
			break
		}
		keep = append(keep, seg)
	}
	p = strings.TrimLeft(path.Clean(strings.Join(keep, "/")), "/")
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "."
	}
	return p
}

// touchesOverlap reports whether a and b can name the same file: one covers
// the other, at a path-segment boundary.
func touchesOverlap(a, b string) bool {
	a, b = touchRoot(a), touchRoot(b)
	return a == "." || b == "." || a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// liveTouches is the declared paths of every live claim: a bead in progress
// with a claim on the ledger (an assignee, a run: label or bops:landing), or
// with a held or landing claim ref. A ref's bead is looked up in the
// in-progress list, then in ready, where a claim the ledger has not caught up
// with still reads open. A bead found in neither declares nothing here.
func liveTouches(inProgress, ready []ledger.Issue, refs map[string]lock.Ref) map[string][]string {
	byID := map[string]*ledger.Issue{}
	for k := range ready {
		byID[ready[k].ID] = &ready[k]
	}
	out := map[string][]string{}
	for k := range inProgress {
		i := &inProgress[k]
		byID[i.ID] = i
		if i.Assignee != "" || len(i.LabelsWithPrefix("run:")) > 0 || i.HasLabel("bops:landing") {
			if t := touches(i); len(t) > 0 {
				out[i.ID] = t
			}
		}
	}
	for id, r := range refs {
		if r.State != lock.StateHeld && r.State != lock.StateLanding {
			continue
		}
		if i := byID[id]; i != nil {
			if t := touches(i); len(t) > 0 {
				out[id] = t
			}
		}
	}
	return out
}

// conflict names the first live claim, other than id itself, whose paths
// overlap mine, as a why_not; "" when none does. Claims are tried in id order
// so the answer is stable.
func conflict(id string, mine []string, live map[string][]string, order []string) string {
	for _, other := range order {
		if other == id {
			continue
		}
		for _, a := range mine {
			for _, b := range live[other] {
				if touchesOverlap(a, b) {
					return fmt.Sprintf("touches: %s overlaps %s of claimed %s", a, b, other)
				}
			}
		}
	}
	return ""
}
