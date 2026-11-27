// Package forge answers questions about merge requests: has one merged, and
// what commit is it at (SPEC.md §7), and, for github and gitlab, which ones a
// text search finds (Searcher). Nothing else in bops knows what a pull
// request is. Forge CLIs are the only way in, so no token handling
// lives here.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Merged is the answer to is_merged.
type Merged string

const (
	Yes     Merged = "true"
	No      Merged = "false"
	Unknown Merged = "unknown"
)

// Forge is the adapter from SPEC §7; search_mrs is the separate Searcher.
type Forge interface {
	Name() string
	// IsMerged returns Unknown rather than failing, so one bad ref cannot
	// stop a sweep.
	IsMerged(ctx context.Context, ref string) Merged
	RefSHA(ctx context.Context, ref string) (string, error)
}

// MR is a merge request a search found. State is open, merged or closed.
type MR struct {
	URL, Title, Branch, State string
}

// ErrIncomplete is returned, with the merge requests found, when a search
// hit its result limit, so a merge request it did not return may exist.
var ErrIncomplete = errors.New("the forge search hit its result limit, so it may have missed some")

// SearchLimit is the most merge requests one search returns.
const SearchLimit = 100

// Searcher is a forge that can search its merge requests. none is not one.
type Searcher interface {
	// SearchMRs returns the merge requests of this repository, in any
	// state, that the forge's text search finds for text (a bead id).
	SearchMRs(ctx context.Context, text string) ([]MR, error)
}

// Runner executes an external command and returns stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec runs a real command.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Select returns the named forge. "auto" or "" infers it from the URL of
// remote, the repository's lock.remote ("" means origin), falling back to
// none when that remote is missing or has no recognisable host. The lock
// remote is used even under lock.mode none: it is the remote the repository
// is configured to share through. A named github or gitlab forge also takes
// its API host from that remote.
func Select(ctx context.Context, name, remote string, run Runner) (Forge, error) {
	if name != "" && name != "auto" && name != "github" && name != "gitlab" && name != "none" {
		return nil, fmt.Errorf("unknown forge %q (have: auto, github, gitlab, none)", name)
	}
	if remote == "" {
		remote = "origin"
	}
	host, repo := "", ""
	if out, err := run(ctx, "git", "remote", "get-url", remote); err == nil {
		host, repo = remoteRepo(strings.TrimSpace(string(out)))
	}
	if name == "" || name == "auto" {
		switch {
		case strings.Contains(host, "github"):
			name = "github"
		case strings.Contains(host, "gitlab"):
			name = "gitlab"
		default:
			name = "none"
		}
	}
	switch name {
	case "github":
		return &github{run: run, host: orDefault(host, "github.com"), repo: repo}, nil
	case "gitlab":
		return &gitlab{run: run, host: orDefault(host, "gitlab.com"), repo: repo}, nil
	}
	return none{}, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// remoteRepo extracts the host and the repository path (owner/name, or
// group/sub/project, without .git) from https://, ssh:// and scp-style
// remotes. A local path has neither; a path with an unsafe segment is "".
func remoteRepo(remote string) (host, repo string) {
	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Scheme == "file" {
			return "", ""
		}
		host, path = u.Hostname(), u.Path
	} else if at := strings.Index(remote, "@"); at >= 0 {
		colon := strings.Index(remote[at:], ":")
		if colon <= 0 {
			return "", ""
		}
		host, path = remote[at+1:at+colon], remote[at+colon+1:]
	} else {
		return "", ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if !safeSegment.MatchString(p) || p == "." || p == ".." {
			return host, ""
		}
	}
	if len(parts) < 2 {
		return host, ""
	}
	return host, path
}

// noRepo is the search error when the remote names no repository.
func noRepo(host string) error {
	return fmt.Errorf("the lock remote's URL names no repository on %s, so its merge requests cannot be searched", host)
}

var (
	safeSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	number      = regexp.MustCompile(`^[0-9]+$`)
)

// segments checks that ref is on host and returns its clean path parts. The
// host check matters: refs come from the ledger, and a forge CLI must not
// be pointed at a server this repository does not use.
func segments(ref, host string) ([]string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("ref %q is not a URL: %w", ref, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("ref %q is not an http(s) URL", ref)
	}
	if u.Hostname() != host {
		return nil, fmt.Errorf("ref %q is not on %s", ref, host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for _, p := range parts {
		if !safeSegment.MatchString(p) || p == "." || p == ".." {
			return nil, fmt.Errorf("ref %q has an invalid path", ref)
		}
	}
	return parts, nil
}

// --- GitHub

type github struct {
	run  Runner
	host string
	repo string // owner/name, from the lock remote's URL
}

func (g *github) Name() string { return "github" }

type githubPull struct {
	Merged bool `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (g *github) fetch(ctx context.Context, ref string) (*githubPull, error) {
	p, err := segments(ref, g.host)
	if err != nil {
		return nil, err
	}
	if len(p) != 4 || p[2] != "pull" || !number.MatchString(p[3]) {
		return nil, fmt.Errorf("ref %q is not a pull request", ref)
	}
	out, err := g.run(ctx, "gh", "api", "--hostname", g.host, fmt.Sprintf("repos/%s/%s/pulls/%s", p[0], p[1], p[3]))
	if err != nil {
		return nil, err
	}
	var pull githubPull
	if err := json.Unmarshal(out, &pull); err != nil {
		return nil, fmt.Errorf("decode pull request: %w", err)
	}
	return &pull, nil
}

func (g *github) IsMerged(ctx context.Context, ref string) Merged {
	pull, err := g.fetch(ctx, ref)
	if err != nil {
		return Unknown
	}
	if pull.Merged {
		return Yes
	}
	return No
}

func (g *github) RefSHA(ctx context.Context, ref string) (string, error) {
	pull, err := g.fetch(ctx, ref)
	if err != nil {
		return "", err
	}
	return pull.Head.SHA, nil
}

type githubSearchHit struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"` // OPEN, CLOSED or MERGED
}

// SearchMRs runs gh pr list --search, the forge's search of titles and
// bodies (not comments), for text as a phrase.
func (g *github) SearchMRs(ctx context.Context, text string) ([]MR, error) {
	if g.repo == "" {
		return nil, noRepo(g.host)
	}
	out, err := g.run(ctx, "gh", "pr", "list", "--repo", g.host+"/"+g.repo, "--state", "all",
		"--search", `"`+strings.ReplaceAll(text, `"`, "")+`" in:title,body`, "--limit", strconv.Itoa(SearchLimit),
		"--json", "url,title,headRefName,state")
	if err != nil {
		return nil, err
	}
	var hits []githubSearchHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("decode pull request search: %w", err)
	}
	mrs := make([]MR, 0, len(hits))
	for _, h := range hits {
		mrs = append(mrs, MR{URL: h.URL, Title: h.Title, Branch: h.HeadRefName, State: strings.ToLower(h.State)})
	}
	if len(hits) >= SearchLimit {
		return mrs, ErrIncomplete
	}
	return mrs, nil
}

// --- GitLab

type gitlab struct {
	run  Runner
	host string
	repo string // group/sub/project, from the lock remote's URL
}

func (g *gitlab) Name() string { return "gitlab" }

type gitlabMR struct {
	State string `json:"state"`
	SHA   string `json:"sha"`
}

func (g *gitlab) fetch(ctx context.Context, ref string) (*gitlabMR, error) {
	p, err := segments(ref, g.host)
	if err != nil {
		return nil, err
	}
	// group/sub/project/-/merge_requests/1
	n := len(p)
	if n < 5 || p[n-3] != "-" || p[n-2] != "merge_requests" || !number.MatchString(p[n-1]) {
		return nil, fmt.Errorf("ref %q is not a merge request", ref)
	}
	project := url.PathEscape(strings.Join(p[:n-3], "/"))
	out, err := g.run(ctx, "glab", "api", "--hostname", g.host, fmt.Sprintf("projects/%s/merge_requests/%s", project, p[n-1]))
	if err != nil {
		return nil, err
	}
	var mr gitlabMR
	if err := json.Unmarshal(out, &mr); err != nil {
		return nil, fmt.Errorf("decode merge request: %w", err)
	}
	return &mr, nil
}

func (g *gitlab) IsMerged(ctx context.Context, ref string) Merged {
	mr, err := g.fetch(ctx, ref)
	if err != nil {
		return Unknown
	}
	if mr.State == "merged" {
		return Yes
	}
	return No
}

func (g *gitlab) RefSHA(ctx context.Context, ref string) (string, error) {
	mr, err := g.fetch(ctx, ref)
	if err != nil {
		return "", err
	}
	return mr.SHA, nil
}

type gitlabSearchHit struct {
	WebURL       string `json:"web_url"`
	Title        string `json:"title"`
	SourceBranch string `json:"source_branch"`
	State        string `json:"state"` // opened, closed, locked or merged
}

// SearchMRs asks the merge requests API to search titles and descriptions
// for text, in every state.
func (g *gitlab) SearchMRs(ctx context.Context, text string) ([]MR, error) {
	if g.repo == "" {
		return nil, noRepo(g.host)
	}
	q := url.Values{"state": {"all"}, "search": {text}, "per_page": {strconv.Itoa(SearchLimit)}}
	out, err := g.run(ctx, "glab", "api", "--hostname", g.host,
		fmt.Sprintf("projects/%s/merge_requests?%s", url.PathEscape(g.repo), q.Encode()))
	if err != nil {
		return nil, err
	}
	var hits []gitlabSearchHit
	if err := json.Unmarshal(out, &hits); err != nil {
		return nil, fmt.Errorf("decode merge request search: %w", err)
	}
	mrs := make([]MR, 0, len(hits))
	for _, h := range hits {
		state := h.State
		if state == "opened" || state == "locked" {
			state = "open"
		}
		mrs = append(mrs, MR{URL: h.WebURL, Title: h.Title, Branch: h.SourceBranch, State: state})
	}
	if len(hits) >= SearchLimit {
		return mrs, ErrIncomplete
	}
	return mrs, nil
}

// --- none

type none struct{}

func (none) Name() string { return "none" }

func (none) IsMerged(context.Context, string) Merged { return Unknown }

func (none) RefSHA(context.Context, string) (string, error) {
	return "", errors.New("no forge configured: cannot verify a ref")
}
