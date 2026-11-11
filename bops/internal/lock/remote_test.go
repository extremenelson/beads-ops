package lock

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// onUpstream is a client whose only remote is named upstream, with the lock
// configured to use it (lock.remote). Every git call it makes is recorded.
func onUpstream(t *testing.T, run string) (*side, string, *[][]string) {
	t.Helper()
	r := bare(t)
	s := client(t, r, run)
	sh(t, s.dir, "remote", "rename", "origin", "upstream")
	s.lock.Remote = "upstream"
	calls := &[][]string{}
	s.git.before = func(c Cmd) (Output, error, bool) {
		*calls = append(*calls, c.Args)
		return Output{}, nil, false
	}
	return s, r, calls
}

func noOriginIn(t *testing.T, calls [][]string) {
	t.Helper()
	for _, c := range calls {
		if slices.Contains(c, "origin") {
			t.Errorf("git %v names origin; the lock must use upstream", c)
		}
	}
}

func TestLockUsesTheConfiguredRemote(t *testing.T) {
	s, r, calls := onUpstream(t, "A")
	if url, err := s.lock.RemoteURL(ctx); err != nil || url != r {
		t.Fatalf("remote url = %q, %v; want %q", url, err, r)
	}
	sha := acquire(t, s, "x-1")
	if got := sh(t, r, "rev-parse", claimsRoot+"x-1"); got != sha {
		t.Errorf("ref on upstream = %s, want %s", got, sha)
	}
	if live, found, err := s.lock.Live(ctx, "x-1"); err != nil || !found || live != sha {
		t.Errorf("live = %s %v %v", live, found, err)
	}
	if ref, err := s.lock.Read(ctx, "x-1"); err != nil || ref == nil || ref.SHA != sha {
		t.Errorf("read = %+v, %v", ref, err)
	}
	if err := s.lock.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := s.lock.Delete(ctx, "x-1", sha); err != nil || res.Outcome != Gone {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	if left := sh(t, r, "for-each-ref", "refs/bops/"); left != "" {
		t.Errorf("ref left on upstream: %s", left)
	}
	noOriginIn(t, *calls)
}

func TestLockRemoteMissingRefusesWithNoOrigin(t *testing.T) {
	s, _, _ := onUpstream(t, "A")
	s.lock.Remote = "mirror"
	_, err := s.lock.RemoteURL(ctx)
	if CodeOf(err) != CodeNoOrigin || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("err = %v, want no_origin naming mirror", err)
	}
	s.lock.Remote = "" // unset falls back to origin, which this clone lacks
	if _, err := s.lock.RemoteURL(ctx); CodeOf(err) != CodeNoOrigin || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("err = %v, want no_origin naming origin", err)
	}
}

func TestAuthFailureNamesTheConfiguredRemote(t *testing.T) {
	s, _, _ := onUpstream(t, "A")
	s.git.before = func(c Cmd) (Output, error, bool) {
		return Output{Stderr: "fatal: could not read Password for 'https://x'"}, errors.New("exit status 128"), true
	}
	_, _, err := s.lock.Live(ctx, "x-1")
	if CodeOf(err) != CodeAuth || !strings.Contains(err.Error(), "git ls-remote upstream") {
		t.Fatalf("err = %v, want git_auth naming upstream", err)
	}
}

func TestDoctorUsesTheConfiguredRemote(t *testing.T) {
	s, r, calls := onUpstream(t, "doctor")
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckOK, "fetch": CheckOK, "push": CheckOK, "lock": CheckOK})
	if d.Remote != r {
		t.Errorf("remote = %q, want %q", d.Remote, r)
	}
	if c := checkOf(t, d, "origin"); !strings.Contains(c.Detail, "upstream") {
		t.Errorf("origin detail = %q, want it to name upstream", c.Detail)
	}
	if left := probeRefs(t, r); left != "" {
		t.Errorf("probe left on the remote: %s", left)
	}
	noOriginIn(t, *calls)
}

func TestDoctorConfiguredRemoteMissing(t *testing.T) {
	s, _, _ := onUpstream(t, "doctor")
	s.lock.Remote = "mirror"
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckFail, "fetch": CheckSkip, "push": CheckSkip, "lock": CheckSkip})
	c := checkOf(t, d, "origin")
	if c.Code != CodeNoOrigin || !strings.Contains(c.Next, "git remote add mirror") || !strings.Contains(c.Next, "lock.remote") {
		t.Errorf("origin = %+v", c)
	}
}
