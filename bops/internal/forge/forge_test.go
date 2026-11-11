package forge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// stub replays canned output for gh, glab and git, and logs the calls.
type stub struct {
	out   map[string]string
	calls []string
}

func (s *stub) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, key)
	if v, ok := s.out[key]; ok {
		return []byte(v), nil
	}
	return nil, errors.New("exit 1")
}

const (
	ghURL = "https://github.com/o/r/pull/3"
	ghKey = "gh api --hostname github.com repos/o/r/pulls/3"
	glURL = "https://gitlab.com/g/sub/p/-/merge_requests/7"
	glKey = "glab api --hostname gitlab.com projects/g%2Fsub%2Fp/merge_requests/7"
)

func TestSelect(t *testing.T) {
	for _, tc := range []struct {
		name, remote, want string
	}{
		{"auto", "https://github.com/o/r.git", "github"},
		{"", "git@github.com:o/r.git", "github"},
		{"auto", "ssh://git@gitlab.example.com/o/r.git", "gitlab"},
		{"auto", "git@gitlab.com:g/p.git", "gitlab"},
		{"auto", "/srv/git/r.git", "none"},
		{"auto", "file:///srv/git/r.git", "none"},
		{"auto", "", "none"},
		{"github", "/srv/git/r.git", "github"},
		{"gitlab", "https://github.com/o/r.git", "gitlab"},
		{"none", "https://github.com/o/r.git", "none"},
	} {
		s := &stub{out: map[string]string{}}
		if tc.remote != "" {
			s.out["git remote get-url origin"] = tc.remote + "\n"
		}
		f, err := Select(context.Background(), tc.name, "origin", s.run)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if f.Name() != tc.want {
			t.Errorf("%+v: got %s", tc, f.Name())
		}
	}
}

func TestSelectRejectsUnknownName(t *testing.T) {
	if _, err := Select(context.Background(), "bitbucket", "origin", (&stub{}).run); err == nil {
		t.Fatal("want an error")
	}
}

// The lock remote (lock.remote) may not be origin; inference reads it.
func TestSelectInfersFromTheNamedRemote(t *testing.T) {
	s := &stub{out: map[string]string{
		"git remote get-url origin":   "https://github.com/o/r.git\n",
		"git remote get-url upstream": "git@gitlab.com:g/p.git\n",
	}}
	f, err := Select(context.Background(), "auto", "upstream", s.run)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "gitlab" {
		t.Errorf("got %s, want gitlab from upstream", f.Name())
	}
	if want := []string{"git remote get-url upstream"}; !reflect.DeepEqual(s.calls, want) {
		t.Errorf("calls = %v, want %v", s.calls, want)
	}
}

// A named remote that does not exist infers none, as a missing origin does,
// and an empty name means origin.
func TestSelectRemoteFallbacks(t *testing.T) {
	s := &stub{out: map[string]string{"git remote get-url origin": "https://github.com/o/r.git\n"}}
	if f, err := Select(context.Background(), "auto", "mirror", s.run); err != nil || f.Name() != "none" {
		t.Errorf("missing remote: got %v, %v; want none", f, err)
	}
	s.calls = nil
	if f, err := Select(context.Background(), "auto", "", s.run); err != nil || f.Name() != "github" {
		t.Errorf("empty remote: got %v, %v; want github from origin", f, err)
	}
	if want := []string{"git remote get-url origin"}; !reflect.DeepEqual(s.calls, want) {
		t.Errorf("calls = %v, want %v", s.calls, want)
	}
}

func TestRemoteHost(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/o/r.git":          "github.com",
		"https://user@ghe.example.com:8443/r": "ghe.example.com",
		"git@github.com:o/r.git":              "github.com",
		"ssh://git@gitlab.example.com:22/o/r": "gitlab.example.com",
		"/srv/git/r.git":                      "",
		"file:///srv/git/r.git":               "",
	} {
		if got := remoteHost(remote); got != want {
			t.Errorf("%s: got %q, want %q", remote, got, want)
		}
	}
}

func TestGitHubIsMerged(t *testing.T) {
	for body, want := range map[string]Merged{
		`{"state":"closed","merged":true,"head":{"sha":"abc"}}`:  Yes,
		`{"state":"open","merged":false,"head":{"sha":"abc"}}`:   No,
		`{"state":"closed","merged":false,"head":{"sha":"abc"}}`: No,
		`not json`: Unknown,
	} {
		g := &github{host: "github.com", run: (&stub{out: map[string]string{ghKey: body}}).run}
		if got := g.IsMerged(context.Background(), ghURL); got != want {
			t.Errorf("%s: got %s, want %s", body, got, want)
		}
	}
}

func TestGitHubRefSHA(t *testing.T) {
	g := &github{host: "github.com", run: (&stub{out: map[string]string{ghKey: `{"merged":true,"head":{"sha":"abc123"}}`}}).run}
	got, err := g.RefSHA(context.Background(), ghURL)
	if err != nil || got != "abc123" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestGitHubCommandFailureIsUnknown(t *testing.T) {
	g := &github{host: "github.com", run: (&stub{}).run}
	if got := g.IsMerged(context.Background(), ghURL); got != Unknown {
		t.Errorf("got %s", got)
	}
}

func TestBadRefsNeverReachTheForgeCLI(t *testing.T) {
	for _, ref := range []string{
		"https://evil.example.com/o/r/pull/3",
		"https://github.com.evil.example/o/r/pull/3",
		"https://github.com/o/r/issues/3",
		"https://github.com/o/r/pull/abc",
		"https://github.com/o/r/pull/3/files",
		"https://github.com/o/../r/pull/3",
		"https://github.com/o/r%2Fx/pull/3",
		"ftp://github.com/o/r/pull/3",
		"--hostname=evil.example.com",
		"not a url",
		"",
	} {
		s := &stub{}
		g := &github{host: "github.com", run: s.run}
		if got := g.IsMerged(context.Background(), ref); got != Unknown {
			t.Errorf("%q: got %s", ref, got)
		}
		if _, err := g.RefSHA(context.Background(), ref); err == nil {
			t.Errorf("%q: RefSHA should fail", ref)
		}
		if len(s.calls) != 0 {
			t.Errorf("%q reached the CLI: %v", ref, s.calls)
		}
	}
}

func TestGitLab(t *testing.T) {
	for body, want := range map[string]Merged{
		`{"state":"merged","sha":"def"}`: Yes,
		`{"state":"opened","sha":"def"}`: No,
		`{"state":"closed","sha":"def"}`: No,
	} {
		g := &gitlab{host: "gitlab.com", run: (&stub{out: map[string]string{glKey: body}}).run}
		if got := g.IsMerged(context.Background(), glURL); got != want {
			t.Errorf("%s: got %s, want %s", body, got, want)
		}
	}
	g := &gitlab{host: "gitlab.com", run: (&stub{out: map[string]string{glKey: `{"state":"merged","sha":"def"}`}}).run}
	if got, err := g.RefSHA(context.Background(), glURL); err != nil || got != "def" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestGitLabRejectsWrongShape(t *testing.T) {
	s := &stub{}
	g := &gitlab{host: "gitlab.com", run: s.run}
	for _, ref := range []string{"https://gitlab.com/p/-/issues/1", "https://gitlab.com/-/merge_requests/1", "https://gitlab.com/p/merge_requests/1"} {
		if got := g.IsMerged(context.Background(), ref); got != Unknown {
			t.Errorf("%s: got %s", ref, got)
		}
	}
	if !reflect.DeepEqual(s.calls, []string(nil)) {
		t.Errorf("calls = %v", s.calls)
	}
}

func TestNone(t *testing.T) {
	f := none{}
	if f.IsMerged(context.Background(), ghURL) != Unknown {
		t.Error("none must answer unknown")
	}
	if _, err := f.RefSHA(context.Background(), ghURL); err == nil {
		t.Error("none cannot verify")
	}
}
