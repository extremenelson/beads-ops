package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doctorSide(t *testing.T) (*side, string) {
	t.Helper()
	r := bare(t)
	s := client(t, r, "doctor")
	return s, r
}

func checkOf(t *testing.T, d *Diagnosis, id string) Check {
	t.Helper()
	for _, c := range d.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", id, d.Checks)
	return Check{}
}

func wantStatuses(t *testing.T, d *Diagnosis, want map[string]string) {
	t.Helper()
	for id, status := range want {
		if c := checkOf(t, d, id); c.Status != status {
			t.Errorf("%s: status %q, want %q (%+v)", id, c.Status, status, c)
		}
	}
}

func probeRefs(t *testing.T, remote string) string {
	t.Helper()
	return sh(t, remote, "for-each-ref", "refs/bops/")
}

func TestDoctorHealthyRemote(t *testing.T) {
	s, r := doctorSide(t)
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckOK, "fetch": CheckOK, "push": CheckOK, "lock": CheckOK})
	if len(d.Failed) != 0 {
		t.Errorf("failed = %v", d.Failed)
	}
	if d.Remote != r {
		t.Errorf("remote = %q, want %q", d.Remote, r)
	}
	if !strings.HasPrefix(d.ProbeRef, "refs/bops/doctor/") {
		t.Errorf("probe ref = %q", d.ProbeRef)
	}
	if left := probeRefs(t, r); left != "" {
		t.Errorf("probe left on the remote: %s", left)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("warnings = %v", d.Warnings)
	}
}

func TestDoctorNoOrigin(t *testing.T) {
	s, _ := doctorSide(t)
	sh(t, s.dir, "remote", "remove", "origin")
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckFail, "fetch": CheckSkip, "push": CheckSkip, "lock": CheckSkip})
	if c := checkOf(t, d, "origin"); c.Code != CodeNoOrigin || c.Next == "" {
		t.Errorf("origin = %+v", c)
	}
	if strings.Join(d.Failed, ",") != "origin" {
		t.Errorf("failed = %v", d.Failed)
	}
}

func TestDoctorUnreachableRemote(t *testing.T) {
	s, _ := doctorSide(t)
	sh(t, s.dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckOK, "fetch": CheckFail, "push": CheckSkip, "lock": CheckSkip})
	if c := checkOf(t, d, "fetch"); c.Code != CodeUnreachable || c.Next == "" {
		t.Errorf("fetch = %+v", c)
	}
}

func TestDoctorFetchAuthFailure(t *testing.T) {
	s, _ := doctorSide(t)
	s.git.before = func(c Cmd) (Output, error, bool) {
		if c.Args[0] == "ls-remote" {
			return Output{Stderr: "fatal: Authentication failed for 'https://user:tok3n@example.invalid/r.git/'"}, errors.New("exit status 128"), true
		}
		return Output{}, nil, false
	}
	d := s.lock.Doctor(ctx)
	c := checkOf(t, d, "fetch")
	if c.Status != CheckFail || c.Code != CodeAuth {
		t.Errorf("fetch = %+v", c)
	}
	if strings.Contains(c.Detail, "tok3n") {
		t.Errorf("credentials leaked: %s", c.Detail)
	}
}

func TestDoctorPushAuthFailure(t *testing.T) {
	s, r := doctorSide(t)
	s.git.before = func(c Cmd) (Output, error, bool) {
		if isPush(c) {
			return Output{Stderr: "ERROR: Permission to o/r.git denied to someone.\nfatal: unable to access: The requested URL returned error: 403"}, errors.New("exit status 128"), true
		}
		return Output{}, nil, false
	}
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckOK, "fetch": CheckOK, "push": CheckFail, "lock": CheckSkip})
	if c := checkOf(t, d, "push"); c.Code != CodeAuth || c.Next == "" {
		t.Errorf("push = %+v", c)
	}
	if left := probeRefs(t, r); left != "" {
		t.Errorf("probe left on the remote: %s", left)
	}
}

// A dry run cannot see server-side ref rules; only the probe write finds them.
func TestDoctorForgeRefusingRefs(t *testing.T) {
	s, r := doctorSide(t)
	hook := filepath.Join(r, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refs/bops is protected >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := s.lock.Doctor(ctx)
	wantStatuses(t, d, map[string]string{"origin": CheckOK, "fetch": CheckOK, "push": CheckOK, "lock": CheckFail})
	if c := checkOf(t, d, "lock"); c.Code != CodeLockUnsupported || c.Next == "" {
		t.Errorf("lock = %+v", c)
	}
}

// A remote that lets a create-only push overwrite a ref cannot arbitrate claims.
func TestDoctorLeaseNotEnforced(t *testing.T) {
	s, r := doctorSide(t)
	creates := 0
	s.git.before = func(c Cmd) (Output, error, bool) {
		if !isPush(c) || strings.Contains(strings.Join(c.Args, " "), "--dry-run") {
			return Output{}, nil, false
		}
		last := c.Args[len(c.Args)-1]
		if strings.HasPrefix(last, ":") {
			return Output{}, nil, false // the cleanup delete
		}
		if creates++; creates == 2 {
			out, err := s.git.Exec.Run(ctx, Cmd{Args: []string{"push", "--porcelain", "--no-verify", "origin", "+" + last}})
			return out, err, true
		}
		return Output{}, nil, false
	}
	d := s.lock.Doctor(ctx)
	if c := checkOf(t, d, "lock"); c.Status != CheckFail || c.Code != CodeLockUnsupported {
		t.Errorf("lock = %+v", c)
	}
	if left := probeRefs(t, r); left != "" {
		t.Errorf("probe left on the remote: %s", left)
	}
}

func TestDoctorReportsAProbeItCouldNotDelete(t *testing.T) {
	s, r := doctorSide(t)
	s.git.before = func(c Cmd) (Output, error, bool) {
		if isPush(c) && strings.HasPrefix(c.Args[len(c.Args)-1], ":") {
			return Output{Stderr: "fatal: the remote end hung up unexpectedly"}, errors.New("exit status 128"), true
		}
		return Output{}, nil, false
	}
	d := s.lock.Doctor(ctx)
	if c := checkOf(t, d, "lock"); c.Status != CheckOK {
		t.Errorf("lock = %+v", c)
	}
	if probeRefs(t, r) == "" {
		t.Fatal("the delete was blocked, so the probe should remain")
	}
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "git push") || !strings.Contains(d.Warnings[0], d.ProbeRef) {
		t.Errorf("warnings = %v", d.Warnings)
	}
}

func TestDoctorNeverTouchesClaimRefs(t *testing.T) {
	s, r := doctorSide(t)
	sha := acquire(t, s, "x-1")
	s.git.before = func(c Cmd) (Output, error, bool) {
		if strings.Contains(strings.Join(c.Args, " "), claimsRoot) && isPush(c) {
			t.Errorf("doctor pushed to a claim ref: %v", c.Args)
		}
		return Output{}, nil, false
	}
	s.lock.Doctor(ctx)
	if got := sh(t, r, "rev-parse", claimsRoot+"x-1"); got != sha {
		t.Errorf("claim ref moved: %s != %s", got, sha)
	}
}
