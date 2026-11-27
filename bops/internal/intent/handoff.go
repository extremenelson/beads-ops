package intent

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/extremenelson/beads-ops/bops/internal/config"
	"github.com/extremenelson/beads-ops/bops/internal/ledger"
	"github.com/extremenelson/beads-ops/bops/internal/lock"
)

// Previous is, on a claim --force that took a bead over, where the last
// attempt stopped, so the new holder does not start from the bead text alone.
// Everything but the holder is read best effort: a read that fails leaves its
// field empty and adds a warning, and never undoes the takeover.
type Previous struct {
	Actor      string `json:"actor,omitempty"`
	Run        string `json:"run,omitempty"`
	AgeMinutes int    `json:"age_minutes"`
	// LastNote is the NEXT: or BLOCKER: paragraph of the newest bops note on
	// the bead by Run, else of the newest by any run, or that note's last
	// paragraph when it has neither. It is ledger text: anyone who can
	// comment on the bead can write it. NoteRun is the run that wrote it.
	LastNote string `json:"last_note,omitempty"`
	NoteRun  string `json:"note_run,omitempty"`
	// Branch is a branch whose name carries the whole bead id, one on a
	// remote first. This checkout's own branch counts only on a remote, since
	// a taker usually makes it before the takeover. Not looked up with
	// lock.mode none.
	Branch string `json:"branch,omitempty"`
	// MR is the newest landed ref, or else the newest merge request URL in
	// the bead's comments or notes.
	MR string `json:"mr,omitempty"`
}

// notePrefix starts the first line of every comment bops note writes.
const notePrefix = "bops:note run="

// handoffClock bounds the handoff reads of a forced claim, which delay its
// answer after the takeover is already written.
const handoffClock = 10 * time.Second

// maxLastNote caps last_note, which is ledger text read into an agent's
// context, in runes.
const maxLastNote = 2000

// handoffSrc is what a takeover read: the bead as it was, and its holder.
type handoffSrc struct {
	cur  *ledger.Issue
	prev *Previous
}

var paragraphBreak = regexp.MustCompile(`\n[ \t]*\n`)

// fillPrevious adds the last note, branch and merge request to p. cur is the
// bead as read before the takeover.
func (e *Env) fillPrevious(ctx context.Context, cur *ledger.Issue, p *Previous, warnings *[]string) {
	warn := func(what string, err error) {
		*warnings = append(*warnings, "previous attempt: "+what+" not read: "+lock.Scrub(err.Error()))
	}
	landed, err := cur.Landed()
	if err != nil {
		warn("landed metadata", err)
	}
	comments, err := e.comments(ctx, cur.ID)
	if err != nil {
		warn("comments", err)
	} else {
		p.LastNote, p.NoteRun = lastNote(comments, p.Run)
	}
	p.MR = newestMR(landed, cur.Notes, comments)
	if e.LockOff || e.Lock == nil {
		return
	}
	branches, err := e.workBranches(ctx)
	if err != nil {
		warn("branches", err)
		return
	}
	here := ""
	if res, err := e.Lock.Git.Run(ctx, lock.Cmd{Args: []string{"symbolic-ref", "--quiet", "--short", "HEAD"}}); err == nil {
		here = strings.TrimSpace(res.Stdout) // empty on a detached HEAD
	}
	p.Branch = pickBranch(branches, cur.ID, here)
}

// newestMR is the newest landed ref, or else the newest merge request URL in
// the comments or, last, the notes.
func newestMR(landed []ledger.Landed, notes string, comments []bdComment) string {
	if len(landed) > 0 {
		return landed[len(landed)-1].Ref
	}
	// Newest first: comments are oldest first, and notes are older than any
	// comment only by convention, so they come last.
	texts := []string{notes}
	for _, c := range comments {
		texts = append(texts, c.Text)
	}
	for k := len(texts) - 1; k >= 0; k-- {
		if urls := mrURL.FindAllString(texts[k], -1); len(urls) > 0 {
			return urls[len(urls)-1]
		}
	}
	return ""
}

// lastNote returns the handoff paragraph of the newest bops note by run, or
// of the newest by any run when run wrote none, and the run that wrote it.
// Newest is by created_at when every note has one, else by position.
func lastNote(comments []bdComment, run string) (paragraph, by string) {
	byTime := true
	for _, c := range comments {
		if strings.HasPrefix(c.Text, notePrefix) && c.CreatedAt.IsZero() {
			byTime = false
		}
	}
	var newest, newestByRun *bdComment
	newer := func(c, than *bdComment) bool {
		return than == nil || !byTime || !c.CreatedAt.Before(than.CreatedAt)
	}
	for k := range comments {
		c := &comments[k]
		if !strings.HasPrefix(c.Text, notePrefix) {
			continue
		}
		if newer(c, newest) {
			newest = c
		}
		if run != "" && noteRun(c.Text) == run && newer(c, newestByRun) {
			newestByRun = c
		}
	}
	if newestByRun != nil {
		newest = newestByRun
	}
	if newest == nil {
		return "", ""
	}
	_, body, _ := strings.Cut(newest.Text, "\n")
	paragraph = handoffParagraph(body)
	if r := []rune(paragraph); len(r) > maxLastNote {
		paragraph = string(r[:maxLastNote]) + " [truncated]"
	}
	return paragraph, noteRun(newest.Text)
}

// noteRun is the run a bops note's header names.
func noteRun(text string) string {
	header, _, _ := strings.Cut(strings.TrimPrefix(text, notePrefix), "\n")
	if f := strings.Fields(header); len(f) > 0 {
		return f[0] // op=<id> may follow the run
	}
	return ""
}

// handoffParagraph is text's last NEXT: or BLOCKER: paragraph, or else its
// last paragraph.
func handoffParagraph(text string) string {
	paragraphs := paragraphBreak.Split(strings.ReplaceAll(text, "\r\n", "\n"), -1)
	last := ""
	for k := len(paragraphs) - 1; k >= 0; k-- {
		p := strings.TrimSpace(paragraphs[k])
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "NEXT:") || strings.HasPrefix(p, "BLOCKER:") {
			return p
		}
		if last == "" {
			last = p
		}
	}
	return last
}

// pickBranch chooses the branch that most likely holds id's work: a name
// carrying the whole id, on a remote as listed now, then on a remote as of
// its last fetch (it may be deleted since), then local. Unlike reap's
// BranchNamesBead it takes no suffix, parent or child match: that errs
// towards a match, where a wrong one here would send the taker to unrelated
// work. here, this checkout's branch, counts only on a remote. Worktree
// directory names are not branches.
func pickBranch(branches []workBranch, id, here string) string {
	best, bestRank := "", 3
	for _, b := range branches {
		if strings.HasPrefix(b.where, "worktree directory ") || !namesWholeID(b.name, id) {
			continue
		}
		rank := 2
		switch {
		case strings.HasSuffix(b.where, "(last fetch)"):
			rank = 1
		case strings.HasPrefix(b.where, "remote "):
			rank = 0
		case b.name == here:
			continue
		}
		if rank < bestRank {
			best, bestRank = b.name, rank
		}
	}
	return best
}

// namesWholeID reports whether name carries id as a token bounded by the
// ends or one of / - _ . on each side, and not as the start of a child's id
// (id followed by a dot and a digit).
func namesWholeID(name, id string) bool {
	s, tok := strings.ToLower(name), strings.ToLower(id)
	for from := 0; ; {
		k := strings.Index(s[from:], tok)
		if k < 0 {
			return false
		}
		k += from
		end := k + len(tok)
		child := end+1 < len(s) && s[end] == '.' && s[end+1] >= '0' && s[end+1] <= '9'
		if config.Bounded(s, k, end) && !child {
			return true
		}
		from = k + 1
	}
}
