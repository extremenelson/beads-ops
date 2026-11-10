// Package forge answers two questions about a merge request: has it merged,
// and what commit is it at (SPEC.md §7). Nothing else in bops knows what a
// pull request is. Forge CLIs are the only way in, so no token handling
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
	"strings"
)

// Merged is the answer to is_merged.
type Merged string

const (
	Yes     Merged = "true"
	No      Merged = "false"
	Unknown Merged = "unknown"
)

// Forge is the two-function adapter from SPEC §7.
type Forge interface {
	Name() string
	// IsMerged returns Unknown rather than failing, so one bad ref cannot
	// stop a sweep.
	IsMerged(ctx context.Context, ref string) Merged
	RefSHA(ctx context.Context, ref string) (string, error)
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

// Select returns the named forge. "auto" or "" infers it from the origin
// remote, falling back to none.
func Select(ctx context.Context, name string, run Runner) (Forge, error) {
	if name != "" && name != "auto" && name != "github" && name != "gitlab" && name != "none" {
		return nil, fmt.Errorf("unknown forge %q (have: auto, github, gitlab, none)", name)
	}
	host := ""
	if out, err := run(ctx, "git", "remote", "get-url", "origin"); err == nil {
		host = remoteHost(strings.TrimSpace(string(out)))
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
		return &github{run: run, host: orDefault(host, "github.com")}, nil
	case "gitlab":
		return &gitlab{run: run, host: orDefault(host, "gitlab.com")}, nil
	}
	return none{}, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// remoteHost extracts the host from https://, ssh:// and scp-style remotes.
// A local path has no host.
func remoteHost(remote string) string {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		return u.Hostname()
	}
	if at := strings.Index(remote, "@"); at >= 0 {
		if colon := strings.Index(remote[at:], ":"); colon > 0 {
			return remote[at+1 : at+colon]
		}
	}
	return ""
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

// --- GitLab

type gitlab struct {
	run  Runner
	host string
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

// --- none

type none struct{}

func (none) Name() string { return "none" }

func (none) IsMerged(context.Context, string) Merged { return Unknown }

func (none) RefSHA(context.Context, string) (string, error) {
	return "", errors.New("no forge configured: cannot verify a ref")
}
