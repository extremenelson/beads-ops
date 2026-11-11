package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// probeRoot holds the refs doctor writes. They sit under refs/bops/, which is
// what a remote must accept (SPEC §11), but outside claims, so nothing that
// reads claims ever sees one.
const probeRoot = "refs/bops/doctor/"

// Check statuses.
const (
	CheckOK   = "ok"
	CheckFail = "fail"
	CheckSkip = "skip" // an earlier check failed, so this one could not run
)

// Check is one thing doctor verified.
type Check struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code,omitempty"`
	Next   string `json:"next,omitempty"`
}

// Diagnosis is what doctor found. ProbeRef names the ref it created and
// deleted on the remote to test the lock.
type Diagnosis struct {
	Remote   string   `json:"remote"`
	ProbeRef string   `json:"probe_ref"`
	Checks   []Check  `json:"checks"`
	Failed   []string `json:"failed"`
	Warnings []string `json:"warnings"`
}

func (d *Diagnosis) add(c Check) bool {
	d.Checks = append(d.Checks, c)
	if c.Status == CheckFail {
		d.Failed = append(d.Failed, c.ID)
	}
	return c.Status == CheckOK
}

func (d *Diagnosis) skip(ids ...string) {
	for _, id := range ids {
		d.Checks = append(d.Checks, Check{ID: id, Status: CheckSkip, Detail: "not run: an earlier check failed"})
	}
}

func failed(id string, err error, next string) Check {
	return Check{ID: id, Status: CheckFail, Detail: Scrub(err.Error()), Code: CodeOf(err), Next: next}
}

// nextFor gives the step that fixes a remote failure, by its code.
func nextFor(code, auth, other string) string {
	if code == CodeAuth {
		return auth
	}
	return other
}

// Doctor checks that origin exists, can be read and written, and supports the
// claim lock: a create-only push under refs/bops/ that a second create-only
// push cannot overwrite. That needs a write, so it creates a probe ref under
// refs/bops/doctor/ and deletes it with a lease. Claim refs are never touched.
func (l *Lock) Doctor(ctx context.Context) *Diagnosis {
	d := &Diagnosis{Checks: []Check{}, Failed: []string{}, Warnings: []string{}}

	url, err := l.RemoteURL(ctx)
	if err != nil {
		d.add(failed("origin", err, "git remote add origin <url>: the lock always uses the remote named origin"))
		d.skip("fetch", "push", "lock")
		return d
	}
	d.Remote = Scrub(url)
	d.add(Check{ID: "origin", Status: CheckOK, Detail: "origin is " + d.Remote})

	if _, _, err := l.lsRemote(ctx, claimsRoot+"*"); err != nil {
		d.add(failed("fetch", err, nextFor(CodeOf(err),
			"fix the credentials for origin: `git ls-remote origin` must work with no prompt (GIT_TERMINAL_PROMPT=0, ssh BatchMode=yes)",
			"check the network and origin's URL: `git ls-remote origin`")))
		d.skip("push", "lock")
		return d
	}
	d.add(Check{ID: "fetch", Status: CheckOK, Detail: "read refs under " + claimsRoot + " from origin"})

	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		d.add(failed("push", &Error{Code: CodeGit, Err: err}, "rerun bops doctor"))
		d.skip("lock")
		return d
	}
	d.ProbeRef = probeRoot + hex.EncodeToString(nonce)
	first, err := l.commit(ctx, "probe")
	if err != nil {
		d.add(failed("push", err, "check that this is a writable git repository: `git status`"))
		d.skip("lock")
		return d
	}

	if err := l.dryRun(ctx, d.ProbeRef, first); err != nil {
		d.add(failed("push", err, nextFor(CodeOf(err),
			"give this account push access to origin, or fix its credentials: `git push --dry-run origin HEAD` must work with no prompt",
			"check the network and origin's URL: `git push --dry-run origin HEAD`")))
		d.skip("lock")
		return d
	}
	d.add(Check{ID: "push", Status: CheckOK, Detail: "origin accepts a push from this account (dry run, nothing written)"})

	d.add(l.probeLock(ctx, d.ProbeRef, first))
	if warning := l.removeProbe(ctx, d.ProbeRef); warning != "" {
		d.Warnings = append(d.Warnings, warning)
	}
	return d
}

func (l *Lock) dryRun(ctx context.Context, ref, sha string) error {
	out, err := l.Git.Run(ctx, Cmd{Args: []string{
		"push", "--dry-run", "--porcelain", "--no-verify", remote,
		"--force-with-lease=" + ref + ":", sha + ":" + ref,
	}})
	if parsePorcelain(out.Stdout, ref) == pushed {
		return nil
	}
	return failure(out, err)
}

// probeLock makes the pushes the lock makes: a create-only push, which must
// succeed, then a second create-only push of another commit, which must not.
func (l *Lock) probeLock(ctx context.Context, ref, first string) Check {
	const unsupported = "the claim lock cannot run against this remote: allow pushes to refs/bops/* with --force-with-lease in the forge's ref rules or protections (SPEC §11)"
	write := "wrote and deleted the probe ref " + ref + " on origin: "
	st, out, err := l.push(ctx, ref, first, "")
	switch st {
	case rejected:
		return Check{ID: "lock", Status: CheckFail, Code: CodeLockUnsupported, Detail: "origin refused a create-only push to " + ref + ": " + Scrub(out.Stdout+" "+out.Stderr), Next: unsupported}
	case noStatus:
		return failed("lock", failure(out, err), "rerun bops doctor; if it keeps failing, check origin with `git push --dry-run origin HEAD`")
	}
	second, cerr := l.commit(ctx, "probe")
	if cerr != nil {
		return failed("lock", cerr, "check that this is a writable git repository: `git status`")
	}
	st, out, err = l.push(ctx, ref, second, "")
	switch st {
	case pushed:
		return Check{ID: "lock", Status: CheckFail, Code: CodeLockUnsupported, Detail: write + "a second create-only push overwrote it, so two runs could both claim one bead", Next: unsupported}
	case noStatus:
		return failed("lock", failure(out, err), "rerun bops doctor")
	}
	return Check{ID: "lock", Status: CheckOK, Detail: write + "a create-only push succeeded and a second one was refused"}
}

// removeProbe deletes the probe with a lease on its current value, and returns
// a warning with the command to run if it is still there.
func (l *Lock) removeProbe(ctx context.Context, ref string) string {
	sha, found, err := l.lsRemote(ctx, ref)
	if err == nil && !found {
		return ""
	}
	if err == nil {
		if st, _, _ := l.push(ctx, ref, "", sha); st == pushed {
			return ""
		}
	}
	return fmt.Sprintf("the probe ref may still be on origin; remove it with `git push --no-verify origin :%s`", ref)
}

// lsRemote reads refs matching pattern on origin and returns the first match.
func (l *Lock) lsRemote(ctx context.Context, pattern string) (sha string, found bool, err error) {
	out, runErr := l.Git.Run(ctx, Cmd{Args: []string{"ls-remote", remote, pattern}})
	if runErr != nil {
		return "", false, failure(out, runErr)
	}
	for _, line := range strings.Split(out.Stdout, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			return f[0], true, nil
		}
	}
	return "", false, nil
}
