// Package lock owns the git ref that decides who holds a bead. Every ref write
// follows one protocol, so no command has to repeat it:
//
//	journal the intended sha --> push --> classify --> record the result
//	                                        |
//	                                        +-- a rejection or a missing reply
//	                                            is settled by re-reading the ref
//
// Refnames and the message format stay inside this package. See
// docs/designs/bops-claim-lock-v0.1.0.md.
package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// DefaultRemote is the lock remote when Lock.Remote is empty (lock.remote).
const DefaultRemote = "origin"

const (
	claimsRoot = "refs/bops/claims/"
	seenRoot   = "refs/bops/seen/"
)

// Outcome is the result of a ref write that reached a decision.
type Outcome string

const (
	// Owned means the ref now holds this attempt's commit.
	Owned Outcome = "owned"
	// Lost means someone else changed or removed the ref first.
	Lost Outcome = "lost"
	// Gone means the ref no longer exists.
	Gone Outcome = "gone"
)

// Stable error codes for agents and for events.jsonl.
const (
	CodeBadID           = "bad_id"
	CodeBadMessage      = "bad_message"
	CodeUnreachable     = "unreachable"
	CodeAuth            = "git_auth"
	CodeLockUnsupported = "lock_unsupported"
	CodeContention      = "ref_contention"
	CodeJournal         = "state_write_failed"
	CodeHoldNotRecorded = "hold_not_recorded"
	CodeGit             = "git_failed"
	CodeNoOrigin        = "no_origin"       // the lock remote is missing (named for the default)
	CodeRemoteMismatch  = "remote_mismatch" // the lock remote is not the Dolt remote
)

// Error carries a stable code. SHA is set when a ref may remain on the remote
// and a person may have to remove it.
type Error struct {
	Code string
	SHA  string
	Err  error
}

func (e *Error) Error() string {
	if e.SHA != "" {
		return fmt.Sprintf("%s: %v (ref left at %s)", e.Code, e.Err, e.SHA)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf returns the stable code of err, or "" if it has none.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Journal records hold state around ref writes. runstate.Store implements it.
type Journal interface {
	Intend(id, sha string) error
	Confirm(id, sha string) error
	Abandon(id string) error
	IntendDelete(id string) error
	Drop(id string) error
	Entry(id string) (runstate.Entry, bool)
}

// Lock performs ref operations for one run.
type Lock struct {
	Git     Git
	Journal Journal
	Run     string
	Actor   string
	Now     func() time.Time
	Remote  string // git remote that holds the refs; empty means DefaultRemote
}

// remote is the git remote every lock call names.
func (l *Lock) remote() string {
	if l.Remote != "" {
		return l.Remote
	}
	return DefaultRemote
}

// RemoteName is the git remote the lock uses, for messages.
func (l *Lock) RemoteName() string { return l.remote() }

// Result describes a ref write that reached a decision.
type Result struct {
	Outcome  Outcome
	SHA      string   // the ref's value when Owned
	Holder   *Ref     // who holds it when Lost, if that could be read
	Warnings []string // a failed update of the local seen copy, for example
}

func (l *Lock) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateID refuses anything that could not be one path segment of a refname.
func ValidateID(id string) error {
	if !validID.MatchString(id) || strings.Contains(id, "..") || strings.HasSuffix(id, ".") || strings.HasSuffix(id, ".lock") {
		return &Error{Code: CodeBadID, Err: fmt.Errorf("%q is not a valid bead id", id)}
	}
	return nil
}

// Acquire creates the ref, succeeding only if it does not exist.
func (l *Lock) Acquire(ctx context.Context, id string) (*Result, error) {
	return l.Replace(ctx, id, "", StateHeld)
}

// Replace writes a new commit to the ref, succeeding only if the ref is still
// at expected. An empty expected means the ref must not exist.
func (l *Lock) Replace(ctx context.Context, id, expected, state string) (*Result, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	sha, err := l.commit(ctx, state)
	if err != nil {
		return nil, err
	}
	if err := l.Journal.Intend(id, sha); err != nil {
		return nil, &Error{Code: CodeJournal, Err: err}
	}
	ref := claimsRoot + id
	for attempt := 0; ; attempt++ {
		st, out, pushErr := l.push(ctx, ref, sha, expected)
		if st == pushed {
			return l.won(ctx, id, sha, expected)
		}
		live, found, err := l.Live(ctx, id)
		if err != nil {
			return nil, err // the outcome is unknown, so the intent stays journaled
		}
		switch {
		case found && live == sha:
			return l.won(ctx, id, sha, expected) // the reply was lost, not the push
		case found && live == expected:
			return nil, l.failure(out, pushErr) // nothing changed and nobody else wrote
		case found:
			return l.lost(ctx, id)
		case expected != "":
			return l.lost(ctx, id) // the ref this run leased on has been deleted
		case st == noStatus && attempt > 0:
			return nil, l.failure(out, pushErr)
		case attempt == 0:
			continue // a holder may have released between the push and the re-read
		}
		if err := l.Journal.Abandon(id); err != nil {
			return nil, &Error{Code: CodeJournal, Err: err}
		}
		return nil, &Error{Code: CodeLockUnsupported, Err: fmt.Errorf("the remote refused %s: %s", ref, Scrub(out.Stdout))}
	}
}

// won records a successful write. If that record cannot be saved a new claim
// would have an owner nobody knows about, so its ref is taken back.
func (l *Lock) won(ctx context.Context, id, sha, expected string) (*Result, error) {
	if err := l.Journal.Confirm(id, sha); err != nil {
		if expected != "" {
			return nil, &Error{Code: CodeHoldNotRecorded, SHA: sha, Err: err}
		}
		_, _, delErr := l.push(ctx, claimsRoot+id, "", sha)
		if live, found, readErr := l.Live(ctx, id); readErr == nil && (!found || live != sha) && delErr == nil {
			return nil, &Error{Code: CodeHoldNotRecorded, Err: err}
		}
		return nil, &Error{Code: CodeHoldNotRecorded, SHA: sha, Err: err}
	}
	res := &Result{Outcome: Owned, SHA: sha}
	if _, err := l.Git.Run(ctx, Cmd{Args: []string{"update-ref", seenRoot + id, sha}}); err != nil {
		res.Warnings = append(res.Warnings, "local seen copy not updated: "+err.Error())
	}
	return res, nil
}

func (l *Lock) lost(ctx context.Context, id string) (*Result, error) {
	if err := l.Journal.Abandon(id); err != nil {
		return nil, &Error{Code: CodeJournal, Err: err}
	}
	res := &Result{Outcome: Lost}
	ref, err := l.Read(ctx, id)
	if err != nil {
		res.Warnings = append(res.Warnings, "could not read the holder: "+err.Error())
		return res, nil
	}
	res.Holder = ref
	return res, nil
}

// Delete removes the ref, succeeding only if it is still at expected. An
// error leaves delete_pending set, so sync can finish it.
func (l *Lock) Delete(ctx context.Context, id, expected string) (*Result, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	if err := l.Journal.IntendDelete(id); err != nil {
		return nil, &Error{Code: CodeJournal, Err: err}
	}
	st, out, pushErr := l.push(ctx, claimsRoot+id, "", expected)
	if st != pushed {
		live, found, err := l.Live(ctx, id)
		switch {
		case err != nil:
			return nil, err
		case found && live == expected:
			return nil, l.failure(out, pushErr)
		case found:
			if err := l.Journal.Drop(id); err != nil {
				return nil, &Error{Code: CodeJournal, Err: err}
			}
			return l.lost(ctx, id)
		}
	}
	if err := l.Journal.Drop(id); err != nil {
		return nil, &Error{Code: CodeJournal, Err: err}
	}
	res := &Result{Outcome: Gone}
	if _, err := l.Git.Run(ctx, Cmd{Args: []string{"update-ref", "-d", seenRoot + id}}); err != nil {
		res.Warnings = append(res.Warnings, "local seen copy not removed: "+err.Error())
	}
	return res, nil
}

// Reconcile settles a journal entry against the ref's current value, so a
// write whose reply never came cannot look like a lost claim.
type Reconcile string

const (
	// Unchanged means there was no write to settle and the ref matches.
	Unchanged Reconcile = "unchanged"
	// Adopted means the intended write did land.
	Adopted Reconcile = "adopted"
	// Kept means the intended write did not land and the old claim stands.
	Kept Reconcile = "kept"
	// Discarded means a write that never landed left nothing to keep.
	Discarded Reconcile = "discarded"
	// Displaced means the ref belongs to someone else or is gone.
	Displaced Reconcile = "displaced"
)

// Settle compares the journal entry for id with live, the ref's current value
// ("" if absent), and updates the journal to match. Displaced entries are left
// for the caller, which reports them. Legacy entries have no ref to compare.
func (l *Lock) Settle(id, live string) (Reconcile, error) {
	e, ok := l.Journal.Entry(id)
	if !ok {
		return Discarded, nil
	}
	var err error
	var out Reconcile
	switch {
	case e.Intended != "" && live == e.Intended:
		out, err = Adopted, l.Journal.Confirm(id, live)
	case e.Phase == runstate.PhasePushing:
		out, err = Discarded, l.Journal.Abandon(id)
	case e.Legacy():
		out = Unchanged
	case e.Intended != "" && live == e.SHA:
		out, err = Kept, l.Journal.Abandon(id)
	case live == e.SHA:
		out = Unchanged
	default:
		out = Displaced
	}
	if err != nil {
		return "", &Error{Code: CodeJournal, Err: err}
	}
	return out, nil
}

// Live returns the ref's value on the remote.
func (l *Lock) Live(ctx context.Context, id string) (sha string, found bool, err error) {
	if err := ValidateID(id); err != nil {
		return "", false, err
	}
	out, runErr := l.Git.Run(ctx, Cmd{Args: []string{"ls-remote", l.remote(), claimsRoot + id}})
	if runErr != nil {
		return "", false, l.failure(out, runErr)
	}
	for _, line := range strings.Split(out.Stdout, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == claimsRoot+id {
			return f[0], true, nil
		}
	}
	return "", false, nil
}

// Read fetches the ref from the remote and returns it, or nil if it is absent.
func (l *Lock) Read(ctx context.Context, id string) (*Ref, error) {
	if _, found, err := l.Live(ctx, id); err != nil {
		return nil, err
	} else if !found {
		return nil, nil
	}
	spec := "+" + claimsRoot + id + ":" + seenRoot + id
	if out, err := l.Git.Run(ctx, Cmd{Args: []string{"fetch", "--no-tags", l.remote(), spec}}); err != nil {
		if strings.Contains(out.Stderr, "couldn't find remote ref") {
			return nil, nil // deleted between the two calls
		}
		return nil, l.localFailure(out, err)
	}
	refs, err := l.seen(ctx, seenRoot+id)
	if err != nil {
		return nil, err
	}
	for i := range refs {
		if refs[i].ID == id {
			return &refs[i], nil
		}
	}
	return nil, nil
}

// Fetch refreshes the local copy of every claim from the remote.
func (l *Lock) Fetch(ctx context.Context) error {
	spec := "+" + claimsRoot + "*:" + seenRoot + "*"
	if out, err := l.Git.Run(ctx, Cmd{Args: []string{"fetch", "--no-tags", "--prune", l.remote(), spec}}); err != nil {
		return l.localFailure(out, err)
	}
	return nil
}

// Seen lists the local copy of every claim, with no network.
func (l *Lock) Seen(ctx context.Context) ([]Ref, error) {
	return l.seen(ctx, strings.TrimSuffix(seenRoot, "/"))
}

func (l *Lock) seen(ctx context.Context, pattern string) ([]Ref, error) {
	out, err := l.Git.Run(ctx, Cmd{Args: []string{"for-each-ref", "--format=%(objectname)%09%(refname)%09%(contents)%00", pattern}})
	if err != nil {
		return nil, l.localFailure(out, err)
	}
	refs := []Ref{}
	for _, rec := range strings.Split(out.Stdout, "\x00") {
		f := strings.SplitN(strings.TrimLeft(rec, "\n"), "\t", 3)
		if len(f) != 3 || !strings.HasPrefix(f[1], seenRoot) {
			continue
		}
		refs = append(refs, Ref{ID: strings.TrimPrefix(f[1], seenRoot), SHA: f[0], Message: ParseMessage(f[2])})
	}
	return refs, nil
}

func (l *Lock) commit(ctx context.Context, state string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", &Error{Code: CodeGit, Err: err}
	}
	body, err := Message{Run: l.Run, Actor: l.Actor, State: state, Nonce: hex.EncodeToString(nonce), At: l.now()}.encode()
	if err != nil {
		return "", err
	}
	tree, runErr := l.Git.Run(ctx, Cmd{Args: []string{"hash-object", "-w", "-t", "tree", "--stdin"}})
	if runErr != nil {
		return "", &Error{Code: CodeGit, Err: fmt.Errorf("%w: %s", runErr, Scrub(tree.Stderr))}
	}
	out, runErr := l.Git.Run(ctx, Cmd{Args: []string{"commit-tree", strings.TrimSpace(tree.Stdout)}, Stdin: body})
	if runErr != nil {
		return "", &Error{Code: CodeGit, Err: fmt.Errorf("%w: %s", runErr, Scrub(out.Stderr))}
	}
	return strings.TrimSpace(out.Stdout), nil
}

type pushStatus int

const (
	noStatus pushStatus = iota
	rejected
	pushed
)

// push never passes -f, which would cancel the lease (measured). --no-verify
// applies to lock pushes only (D19). An empty sha deletes the ref.
func (l *Lock) push(ctx context.Context, ref, sha, expected string) (pushStatus, Output, error) {
	out, err := l.Git.Run(ctx, Cmd{Args: []string{
		"push", "--porcelain", "--no-verify", l.remote(),
		"--force-with-lease=" + ref + ":" + expected,
		sha + ":" + ref,
	}})
	return parsePorcelain(out.Stdout, ref), out, err
}

// parsePorcelain reads git's "<flag>\t<from>:<to>\t<summary>" status line.
func parsePorcelain(stdout, ref string) pushStatus {
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || len(f[0]) != 1 || !strings.HasSuffix(f[1], ":"+ref) {
			continue
		}
		switch f[0] {
		case "*", "+", "-":
			return pushed
		default:
			return rejected
		}
	}
	return noStatus
}

var authSigns = []string{
	"authentication failed",
	"could not read username",
	"could not read password",
	"permission denied (publickey",
	"terminal prompts disabled",
	"invalid username or token",
	"403",
}

// failure classifies a remote git call that did not reach a decision.
func (l *Lock) failure(out Output, err error) error {
	stderr := strings.ToLower(out.Stderr)
	for _, sign := range authSigns {
		if strings.Contains(stderr, sign) {
			return &Error{Code: CodeAuth, Err: fmt.Errorf("git could not authenticate to %s: %s; check credentials with `git ls-remote %s`", l.remote(), Scrub(out.Stderr), l.remote())}
		}
	}
	if err == nil {
		err = errors.New("no status from git")
	}
	return &Error{Code: CodeUnreachable, Err: fmt.Errorf("%w: %s", err, Scrub(out.Stderr))}
}

// localFailure also separates contention on a local ref from a network fault.
func (l *Lock) localFailure(out Output, err error) error {
	if strings.Contains(out.Stderr, "cannot lock ref") {
		return &Error{Code: CodeContention, Err: fmt.Errorf("%w: %s", err, Scrub(out.Stderr))}
	}
	return l.failure(out, err)
}

// RemoteURL returns the URL the lock pushes to. A missing lock remote is
// CodeNoOrigin, whatever the remote is named.
func (l *Lock) RemoteURL(ctx context.Context) (string, error) {
	out, err := l.Git.Run(ctx, Cmd{Args: []string{"remote", "get-url", l.remote()}})
	if err != nil {
		return "", &Error{Code: CodeNoOrigin, Err: fmt.Errorf("the lock needs a git remote named %s (lock.remote): %s", l.remote(), Scrub(out.Stderr))}
	}
	return strings.TrimSpace(out.Stdout), nil
}

var scpURL = regexp.MustCompile(`^(?:[^@/]+@)?([^/:]+):(.+)$`)
var schemeURL = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://(?:[^@/]+@)?([^/:]+)(?::\d+)?(/.*)$`)

// normalizeRemote reduces a URL to host/path, so https and ssh spellings of one
// repository compare equal. Dolt writes the same URL with a git+ prefix.
func normalizeRemote(u string) string {
	u = strings.TrimPrefix(strings.TrimSpace(u), "git+")
	switch {
	case strings.HasPrefix(u, "file://"):
		u = strings.TrimPrefix(u, "file://")
	case schemeURL.MatchString(u):
		m := schemeURL.FindStringSubmatch(u)
		u = strings.ToLower(m[1]) + "/" + strings.TrimLeft(m[2], "/")
	case !strings.HasPrefix(u, "/") && scpURL.MatchString(u):
		m := scpURL.FindStringSubmatch(u)
		u = strings.ToLower(m[1]) + "/" + strings.TrimLeft(m[2], "/")
	}
	return strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
}

// SameRemote reports whether two URLs name one repository.
func SameRemote(a, b string) bool { return normalizeRemote(a) == normalizeRemote(b) }

var urlCredentials = regexp.MustCompile(`://[^/@\s]+@`)

// Scrub removes credentials embedded in remote URLs from git's output.
func Scrub(s string) string {
	return strings.TrimSpace(urlCredentials.ReplaceAllString(s, "://***@"))
}
