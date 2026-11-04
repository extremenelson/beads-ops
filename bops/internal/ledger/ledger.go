// Package ledger holds the beads data types bops reads, and the decoding
// rules for bd's JSON output. Field names follow bd 1.3.x `--json` output.
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Issue is a bead as returned by `bd ready|list|show --json`.
// Only fields bops reads are declared; unknown fields are ignored.
type Issue struct {
	ID                 string       `json:"id"`
	Title              string       `json:"title"`
	Description        string       `json:"description,omitempty"`
	AcceptanceCriteria string       `json:"acceptance_criteria,omitempty"`
	Status             string       `json:"status"`
	Priority           int          `json:"priority"`
	IssueType          string       `json:"issue_type"`
	Assignee           string       `json:"assignee,omitempty"`
	Owner              string       `json:"owner,omitempty"`
	Labels             []string     `json:"labels,omitempty"`
	Notes              string       `json:"notes,omitempty"`
	Parent             string       `json:"parent,omitempty"`
	Dependencies       []Dependency `json:"dependencies,omitempty"`
	DependencyCount    int          `json:"dependency_count"`
	DependentCount     int          `json:"dependent_count"`
	CommentCount       int          `json:"comment_count"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	StartedAt          *time.Time   `json:"started_at,omitempty"`
	LeaseExpiresAt     *time.Time   `json:"lease_expires_at,omitempty"`
	HeartbeatAt        *time.Time   `json:"heartbeat_at,omitempty"`
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

// DecodeIssues parses the output of a list-shaped bd command.
//
// Accepted today: a raw JSON array, or an error object. bd 2.0 will wrap
// output in an envelope (BD_JSON_ENVELOPE=1 opts in early); support lands
// here once its shape is pinned by a fixture — see testdata/README.md.
func DecodeIssues(b []byte) ([]Issue, error) {
	t := strings.TrimSpace(string(b))
	if t == "" {
		return nil, nil
	}
	if strings.HasPrefix(t, "{") {
		var e bdError
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, &ErrBD{e.Error, e.Hint}
		}
		return nil, errors.New("ledger: unexpected object from bd (envelope mode is not supported yet)")
	}
	var out []Issue
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("ledger: decode issues: %w", err)
	}
	return out, nil
}

// DecodeIssue parses `bd show <id> --json`, which returns a one-element
// array on 1.3.x; a bare object is also accepted.
func DecodeIssue(b []byte) (*Issue, error) {
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
