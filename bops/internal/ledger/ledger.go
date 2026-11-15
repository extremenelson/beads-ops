// Package ledger holds the beads data types bops reads, and the decoding
// rules for bd's JSON output. Field names follow bd 1.3.x `--json` output.
package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Issue is a bead as returned by `bd ready|list|show --json`.
// Only fields bops reads are declared; unknown fields are ignored.
type Issue struct {
	ID                 string          `json:"id"`
	Title              string          `json:"title"`
	Description        string          `json:"description,omitempty"`
	AcceptanceCriteria string          `json:"acceptance_criteria,omitempty"`
	Status             string          `json:"status"`
	Priority           int             `json:"priority"`
	IssueType          string          `json:"issue_type"`
	Assignee           string          `json:"assignee,omitempty"`
	Owner              string          `json:"owner,omitempty"`
	Labels             []string        `json:"labels,omitempty"`
	Notes              string          `json:"notes,omitempty"`
	Parent             string          `json:"parent,omitempty"`
	Dependencies       []Dependency    `json:"dependencies,omitempty"`
	DependencyCount    int             `json:"dependency_count"`
	DependentCount     int             `json:"dependent_count"`
	CommentCount       int             `json:"comment_count"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	LeaseExpiresAt     *time.Time      `json:"lease_expires_at,omitempty"`
	HeartbeatAt        *time.Time      `json:"heartbeat_at,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`

	// Gate fields (issue_type gate, from `bd gate list --json`): what the
	// gate waits on. AwaitID is null for human and timer gates; Timeout is
	// in nanoseconds.
	AwaitType string `json:"await_type,omitempty"`
	AwaitID   string `json:"await_id,omitempty"`
	Timeout   int64  `json:"timeout,omitempty"`
	// BlockedBy is the open blockers' ids, from `bd blocked --json`.
	BlockedBy []string `json:"blocked_by,omitempty"`
	// DependencyType is the edge type, from `bd dep list <id> --json`.
	DependencyType string `json:"dependency_type,omitempty"`
}

// Landed is one entry of metadata.bops.landed (SPEC §3.3).
type Landed struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	Run string `json:"run"`
	At  string `json:"at"`
}

func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Landed returns this bead's landing records, oldest first.
func (i *Issue) Landed() ([]Landed, error) {
	top, err := object(i.Metadata)
	if err != nil {
		return nil, fmt.Errorf("ledger: metadata of %s: %w", i.ID, err)
	}
	bops, err := object(top["bops"])
	if err != nil {
		return nil, fmt.Errorf("ledger: metadata.bops of %s: %w", i.ID, err)
	}
	if len(bops["landed"]) == 0 {
		return nil, nil
	}
	var out []Landed
	if err := json.Unmarshal(bops["landed"], &out); err != nil {
		return nil, fmt.Errorf("ledger: metadata.bops.landed of %s: %w", i.ID, err)
	}
	return out, nil
}

// MergeLanded adds l to metadata.bops.landed and leaves every other key
// alone. When the same ref and sha are already recorded it returns that
// entry and no new metadata.
func MergeLanded(i *Issue, l Landed, contract int) (merged []byte, existing *Landed, err error) {
	landed, err := i.Landed()
	if err != nil {
		return nil, nil, err
	}
	for n := range landed {
		if landed[n].Ref == l.Ref && landed[n].SHA == l.SHA {
			return nil, &landed[n], nil
		}
	}
	top, err := object(i.Metadata)
	if err != nil {
		return nil, nil, err
	}
	bops, err := object(top["bops"])
	if err != nil {
		return nil, nil, err
	}
	if bops["landed"], err = json.Marshal(append(landed, l)); err != nil {
		return nil, nil, err
	}
	if bops["contract"], err = json.Marshal(contract); err != nil {
		return nil, nil, err
	}
	if top["bops"], err = json.Marshal(bops); err != nil {
		return nil, nil, err
	}
	merged, err = json.Marshal(top)
	return merged, nil, err
}

// Dependency is one edge as embedded in an Issue.
type Dependency struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"` // blocks, parent-child, discovered-from, related, ...
}

// Blocking dependency types per beads docs (bd 1.3).
var blockingTypes = map[string]bool{"blocks": true, "parent-child": true, "conditional-blocks": true, "waits-for": true}

// IsBlocking reports whether the edge type gates readiness.
func (d Dependency) IsBlocking() bool { return blockingTypes[d.Type] }

// HasLabel reports an exact label match.
func (i *Issue) HasLabel(l string) bool {
	for _, x := range i.Labels {
		if x == l {
			return true
		}
	}
	return false
}

// LabelsWithPrefix returns the values of labels of the form prefix+value.
func (i *Issue) LabelsWithPrefix(prefix string) []string {
	var out []string
	for _, x := range i.Labels {
		if strings.HasPrefix(x, prefix) {
			out = append(out, strings.TrimPrefix(x, prefix))
		}
	}
	return out
}

// bdError is the `{"error": "...", "hint": "..."}` object bd emits on failure.
type bdError struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"`
}

// ErrBD wraps an error object returned by bd.
type ErrBD struct{ Msg, Hint string }

func (e *ErrBD) Error() string {
	if e.Hint != "" {
		return "bd: " + e.Msg + " (" + e.Hint + ")"
	}
	return "bd: " + e.Msg
}

// Unwrap returns the payload of bd's JSON envelope: an object whose only keys
// are data and schema_version, which bd 1.3.x prints with BD_JSON_ENVELOPE=1
// and bd 2.0 will print by default. Anything else, raw-mode output included,
// is returned unchanged. Raw-mode objects that carry schema_version beside
// their own fields (comments add, errors) are not envelopes.
func Unwrap(b []byte) []byte {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || t[0] != '{' {
		return b
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(t, &env) != nil || len(env) != 2 {
		return b
	}
	data, hasData := env["data"]
	_, hasVersion := env["schema_version"]
	if !hasData || !hasVersion {
		return b
	}
	return data
}

// DecodeIssues parses the output of a list-shaped bd command: a JSON array
// of issues, or an error object, in raw or envelope form.
func DecodeIssues(b []byte) ([]Issue, error) {
	b = Unwrap(b)
	t := strings.TrimSpace(string(b))
	if t == "" {
		return nil, nil
	}
	if strings.HasPrefix(t, "{") {
		var e bdError
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, &ErrBD{e.Error, e.Hint}
		}
		return nil, errors.New("ledger: unexpected object from bd where a list of issues was expected")
	}
	var out []Issue
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("ledger: decode issues: %w", err)
	}
	return out, nil
}

// DecodeIssue parses `bd show <id> --json`, which returns a one-element
// array on 1.3.x; a bare object is also accepted, as is the envelope form of
// either.
func DecodeIssue(b []byte) (*Issue, error) {
	b = Unwrap(b)
	t := strings.TrimSpace(string(b))
	if strings.HasPrefix(t, "{") {
		var e bdError
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, &ErrBD{e.Error, e.Hint}
		}
		var one Issue
		if err := json.Unmarshal(b, &one); err != nil {
			return nil, fmt.Errorf("ledger: decode issue: %w", err)
		}
		return &one, nil
	}
	many, err := DecodeIssues(b)
	if err != nil {
		return nil, err
	}
	if len(many) == 0 {
		return nil, &ErrBD{Msg: "no issue returned"}
	}
	return &many[0], nil
}

// DecodeCommentID parses `bd comments add --json`, an object with the new
// comment's id, in raw or envelope form.
func DecodeCommentID(b []byte) (string, error) {
	b = Unwrap(b)
	var c struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("ledger: decode comment: %w", err)
	}
	if c.ID == "" {
		return "", errors.New("ledger: bd returned a comment with no id")
	}
	return c.ID, nil
}
