package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// An operation id makes a write safe to repeat. note, release and land put
// op=<id> in the first line of the one event comment they write, so a run of
// the same operation that finds that comment writes nothing more and reports
// the earlier result with replayed set. A caller that lost an answer, or hit
// a transient failure (a busy bead, a rejected push, a timeout), runs the
// command again instead of reading the bead to see whether it landed.
//
// The id is --op-id when given, else derived from the command, bead, run and
// content: a repeat of the same text by the same run is the same operation.
// A caller that means to write the same text twice passes distinct ids.

// opIDPattern is what --op-id accepts: it goes on an event line, which may be
// a bd argument (SPEC §1.7), so no spaces, quotes or shell characters.
var opIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Event comment kinds that carry an op id.
const (
	evNote     = "bops:note"
	evReleased = "bops:released"
	evLanded   = "bops:landed"
)

// opID returns given, checked, or the derived id of cmd on id by run with
// content parts.
func opID(given, cmd, id, run string, parts ...string) (string, error) {
	if given != "" {
		if !opIDPattern.MatchString(given) {
			return "", fmt.Errorf("%w: op id %q must be 1-64 letters, digits, '.', '_', ':' or '-', starting with a letter or digit", ErrPrecondition, given)
		}
		return given, nil
	}
	h := sha256.New()
	for _, p := range append([]string{cmd, id, run}, parts...) {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "d-" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// recordedOp is an event comment an earlier run of an operation wrote.
type recordedOp struct {
	comment bdComment
	fields  map[string]string // key=value fields of its first line
}

// eventFields parses a comment's first line as `<kind> k=v k=v ...`.
func eventFields(text string) (kind string, fields map[string]string) {
	header, _, _ := strings.Cut(text, "\n")
	parts := strings.Fields(header)
	fields = map[string]string{}
	if len(parts) == 0 {
		return "", fields
	}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			fields[k] = v
		}
	}
	return parts[0], fields
}

// findOp returns the kind event comment on id that this run wrote with op,
// or nil. Only this run's comments count: another run that picked the same
// --op-id, or a comment anyone could have added, never stands in for a write.
func (e *Env) findOp(ctx context.Context, id, kind, op string) (*recordedOp, error) {
	comments, err := e.comments(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		if k, f := eventFields(c.Text); k == kind && f["op"] == op && f["run"] == e.Run.ID {
			return &recordedOp{comment: c, fields: f}, nil
		}
	}
	return nil, nil
}

// replayNote says why a write did nothing.
func replayNote(op string) string {
	return "op " + op + " already recorded on the bead; nothing written, this is its earlier result"
}
