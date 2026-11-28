package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/bdx"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
	"github.com/extremenelson/beads-ops/bops/internal/runstate"
)

// Under lock.mode ref a claim writes its display to the local ledger and
// does not push it: the claim ref decides, and the display rides on this
// machine's next ledger push (docs/designs/ledger-push-contention.md). What
// that leaves unseen is reported here: a claim's failed pull, every push's
// outcome and tries, and a local ledger the remote has not caught up with.

// pushEvent logs one ledger_push line for a pushLedger call that made tries
// tries and ended with err.
func (e *Env) pushEvent(tries int, err error) {
	if e.Store == nil {
		return
	}
	ev := runstate.Event{At: e.now(), Command: e.Command, Step: "ledger_push", Outcome: "pushed", Tries: tries}
	if e.Run != nil {
		ev.Run = e.Run.ID
	}
	if err != nil {
		ev.Outcome, ev.Code, ev.Detail = "failed", lock.CodeOf(err), lock.Scrub(err.Error())
	}
	e.Store.Log(ev)
}

// claimPull is the ledger pull on a claim path (claim, claim --force,
// claim --resume). A pull that fails, other than on a merge conflict
// needing a person (doltPull records that), is logged as a ledger_pull
// event with outcome failed and recorded for status and doctor: the claim
// goes on without it, and since it does not push, no later step of the
// claim would show the failure. cmd names the command in the event. record
// false logs the event only, for claim --force under the lock, which fails
// rather than carry on without its pull, so there is no claim to check.
func (e *Env) claimPull(ctx context.Context, cmd, id string, record bool) error {
	start := e.now()
	_, err := e.doltPull(ctx)
	if err == nil || isLedgerConflict(err) {
		return err
	}
	f := runstate.LedgerPullFailure{At: start, Command: cmd, Bead: id, Detail: lock.Scrub(err.Error())}
	if e.Run != nil {
		f.Run = e.Run.ID
	}
	if e.Store != nil {
		ev := runstate.Event{At: start, Command: cmd, Bead: id, Step: "ledger_pull", Outcome: "failed", Code: lock.CodeOf(err), Detail: f.Detail}
		if e.Run != nil {
			ev.Run = e.Run.ID
		}
		e.Store.Log(ev)
		if record {
			_ = e.Store.SetLedgerPullFailure(&f) // the event is logged either way
		}
	}
	return err
}

// clearPullFailure removes a recorded claim-path pull failure once a pull
// that started at start succeeds and either the claiming run no longer
// holds the bead (nothing is left to check) or the bead passes the re-check
// the claim skipped: after the pull, the ledger shows no other run's claim
// on it and it is not closed. A held bead that fails it (or cannot be read)
// keeps the record, so status and doctor go on saying the claim needs a look. A
// record of a pull that started later (another process sharing the clone)
// is kept too.
func (e *Env) clearPullFailure(ctx context.Context, start time.Time) {
	if e.Store == nil {
		return
	}
	f, err := e.Store.LedgerPullFailure()
	if err == nil && (f == nil || f.At.After(start)) {
		return
	}
	if f != nil && f.Bead != "" && e.stillHolds(f.Run, f.Bead) {
		cur, err := e.show(ctx, f.Bead)
		if err != nil || cur.Status == "closed" {
			return
		}
		for _, r := range cur.LabelsWithPrefix("run:") {
			if r != f.Run {
				return
			}
		}
	}
	if err := e.Store.SetLedgerPullFailure(nil); err != nil {
		return // the next pull that succeeds tries again
	}
	ev := runstate.Event{At: e.now(), Command: e.Command, Step: "ledger_pull", Outcome: "recovered"}
	if f != nil {
		ev.Bead = f.Bead
	}
	if e.Run != nil {
		ev.Run = e.Run.ID
	}
	e.Store.Log(ev)
}

// stillHolds reports whether run of this clone still holds id. A claim it
// no longer holds (released, landed, lost) leaves nothing to re-check.
func (e *Env) stillHolds(run, id string) bool {
	for _, h := range e.holds() {
		if h.ID == id && h.Run == run && !h.Entry.Given() {
			return true
		}
	}
	return false
}

// pullFailureNext is the step after a claim-path pull failed.
const pullFailureNext = "the claim was made without pulling the ledger or re-checking the bead after it; run `bops sync`: a pull that succeeds clears this once the bead shows no other run's claim and is not closed, or once the claim is released; if it stays, read the bead (`bd show <id>`) and release the claim if someone else holds it or it is closed"

// DescribePullFailure says which claim's pull failed, and when.
func DescribePullFailure(f *runstate.LedgerPullFailure) string {
	by := "a claim's ledger pull"
	if f.Command != "" {
		by = "bops " + f.Command + "'s ledger pull"
	}
	if f.Bead != "" {
		by += " for " + f.Bead
	}
	return fmt.Sprintf("%s at %s failed (%s)", by, f.At.UTC().Format(time.RFC3339), f.Detail)
}

// PullFailureReport is a recorded claim-path pull failure with its next
// step, for status.
type PullFailureReport struct {
	runstate.LedgerPullFailure
	Next string `json:"next"`
}

func (e *Env) pullFailureReport(warnings *[]string) *PullFailureReport {
	if e.Store == nil {
		return nil
	}
	f, err := e.Store.LedgerPullFailure()
	if err != nil {
		*warnings = append(*warnings, "ledger pull failure record not read: "+lock.Scrub(err.Error()))
		return nil
	}
	if f == nil {
		return nil
	}
	*warnings = append(*warnings, "ledger not pulled: "+DescribePullFailure(f)+"; next: "+pullFailureNext)
	return &PullFailureReport{LedgerPullFailure: *f, Next: pullFailureNext}
}

// PullFailureWarning is doctor's report of a recorded claim-path pull
// failure in the clone holding dir, or "" when there is none. It only
// reads the record. A failed pull is a warning, not a failed check: the
// claim holds, and the next pull that succeeds clears it.
func PullFailureWarning(dir string) string {
	f, err := runstate.ReadLedgerPullFailure(dir)
	switch {
	case err != nil:
		return "ledger pull failure record not read: " + lock.Scrub(err.Error())
	case f == nil:
		return ""
	}
	return "ledger not pulled: " + lock.Scrub(DescribePullFailure(f)) + "; next: " + pullFailureNext
}

// ------------------------------------------------- unpublished ledger

// publicationWait bounds the bd call that reads how far the local ledger
// is ahead of its Dolt remote. It fetches, so it waits on the network.
const publicationWait = 15 * time.Second

// LedgerRemote is how far the local ledger's main is from one Dolt remote's
// tracking branch. Ahead counts local commits the remote lacks (claim
// displays not yet pushed, among others), Behind the remote's commits not
// yet pulled; -1 is unknown: bd has no tracking branch to count against
// (the remote was never fetched, or has no branch yet). Fetched says bd
// fetched the remote first, so both are as of the read; otherwise they are
// as of the clone's last pull or fetch, and FetchError says why the fetch
// failed.
type LedgerRemote struct {
	Name       string `json:"name"`
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	Fetched    bool   `json:"fetched"`
	FetchError string `json:"fetch_error,omitempty"`
}

// LedgerPublish is what of the local ledger its remote has not seen:
// Uncommitted rows in the working set, and the distance to each Dolt
// remote on the lock remote's repository (the one bops pushes to).
type LedgerPublish struct {
	Uncommitted int            `json:"uncommitted"`
	Remotes     []LedgerRemote `json:"remotes"`
}

// ReadLedgerPublish asks bd how far the local ledger is from the Dolt remote
// at lockURL (every Dolt remote when lockURL is empty). bd 1.3.4 has no
// ahead/behind command and `bd sql` is not supported in embedded mode;
// `bd federation status --json` fetches each Dolt remote and counts
// LocalAhead and LocalBehind against its tracking branch, keeping the last
// counts (Reachable false) when the fetch fails and answering -1 when there
// is no tracking branch, and counts uncommitted rows in pendingChanges. A
// label-only or comment-only commit counts, where `bd diff` would miss it.
// A schema_version other than 1, or a peer with no name, is an error rather
// than a report of zeros.
func ReadLedgerPublish(ctx context.Context, bd bdx.Runner, lockURL string) (*LedgerPublish, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationWait)
	defer cancel()
	raw, err := bd.Run(ctx, "federation", "status", "--json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Peers []struct {
			URL        string `json:"URL"`
			ReachError string `json:"ReachError"`
			Reachable  bool   `json:"Reachable"`
			Status     struct {
				LocalAhead  int    `json:"LocalAhead"`
				LocalBehind int    `json:"LocalBehind"`
				Peer        string `json:"Peer"`
			} `json:"Status"`
		} `json:"peers"`
		PendingChanges int  `json:"pendingChanges"`
		SchemaVersion  *int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("bd federation status --json did not decode: %w", err)
	}
	if doc.SchemaVersion != nil && *doc.SchemaVersion != 1 {
		return nil, fmt.Errorf("bd federation status --json has schema_version %d; this bops reads 1", *doc.SchemaVersion)
	}
	p := &LedgerPublish{Uncommitted: doc.PendingChanges, Remotes: []LedgerRemote{}}
	for _, peer := range doc.Peers {
		if lockURL != "" && !lock.SameRemote(lockURL, peer.URL) {
			continue // a mirror or backup bops does not push to
		}
		if peer.Status.Peer == "" {
			return nil, errors.New("bd federation status --json lists a remote with no name: its shape is not the one this bops reads")
		}
		r := LedgerRemote{Name: peer.Status.Peer, Ahead: peer.Status.LocalAhead, Behind: peer.Status.LocalBehind, Fetched: peer.Reachable}
		if !peer.Reachable {
			r.FetchError = lock.Scrub(firstLine(strings.TrimSpace(peer.ReachError))) // bd runs on with credential hints
		}
		p.Remotes = append(p.Remotes, r)
	}
	return p, nil
}

// publishNext is the step that publishes the local ledger.
const publishNext = "`bops sync` pushes it (as does this machine's next land, release, or a reap or sweep that acts); until then other machines' stale, reap, status and raw bd ready do not see these claims"

// Warnings says what of the ledger is not published, one line per finding:
// uncommitted rows, no Dolt remote on the lock remote's repository, a
// remote the ledger is ahead of (unpublished) or both ahead of and behind
// (diverged), counts bd could not give, and a remote that did not answer.
func (p *LedgerPublish) Warnings() []string {
	var out []string
	if p.Uncommitted > 0 {
		out = append(out, fmt.Sprintf("ledger has %d uncommitted change(s): the next push commits them first; %s", p.Uncommitted, publishNext))
	}
	if len(p.Remotes) == 0 {
		out = append(out, "ledger publication unknown: bd lists no Dolt remote on the lock remote's repository, so nothing this machine writes reaches other machines; "+noDoltRemoteFix)
	}
	for _, r := range p.Remotes {
		fresh := "as of now"
		if !r.Fetched {
			fresh = "as of the last pull or fetch (the remote did not answer: " + r.FetchError + ")"
		}
		switch {
		case r.Ahead < 0 || r.Behind < 0:
			why := ""
			if !r.Fetched {
				why = "; the remote did not answer: " + r.FetchError
			}
			out = append(out, fmt.Sprintf("ledger publication unknown: bd has no tracking branch for Dolt remote %s to count against (never fetched, or the remote has no branch yet)%s; %s", r.Name, why, publishNext))
		case r.Ahead > 0 && r.Behind > 0:
			out = append(out, fmt.Sprintf("ledger diverged from Dolt remote %s, %s: %d local commit(s) it lacks and %d of its not pulled; %s", r.Name, fresh, r.Ahead, r.Behind, publishNext))
		case r.Ahead > 0:
			out = append(out, fmt.Sprintf("ledger unpublished: %d local commit(s) Dolt remote %s lacks, %s; %s", r.Ahead, r.Name, fresh, publishNext))
		case !r.Fetched:
			out = append(out, fmt.Sprintf("ledger publication not checked against Dolt remote %s: it did not answer (%s); as of the last pull or fetch nothing was unpublished", r.Name, r.FetchError))
		}
	}
	return out
}

// ledgerPublish reads the publication report for status under lock.mode
// ref; with the lock off a claim still pushes, and status reads no remote.
// fetched false (the claim-ref fetch failed) skips it: the remote will not
// answer bd's fetch either, and status should not spend its budget, and
// hold the shared store, waiting for it.
func (e *Env) ledgerPublish(ctx context.Context, fetched bool, warnings *[]string) *LedgerPublish {
	if e.LockOff || e.Lock == nil {
		return nil
	}
	if !fetched {
		*warnings = append(*warnings, "ledger publication not read: the lock remote did not answer the fetch")
		return nil
	}
	url, err := e.Lock.RemoteURL(ctx)
	if err != nil {
		*warnings = append(*warnings, "ledger publication not read: "+lock.Scrub(err.Error()))
		return nil
	}
	p, err := ReadLedgerPublish(ctx, e.BD, url)
	if err != nil {
		*warnings = append(*warnings, "ledger publication not read: "+lock.Scrub(err.Error()))
		return nil
	}
	*warnings = append(*warnings, p.Warnings()...)
	return p
}
