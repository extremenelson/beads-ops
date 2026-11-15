package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// buildVCS is the commit this binary was built from, as the go command
// records it when it builds inside a git checkout (`go install ./cmd/bops`
// from bops/). Revision is empty for a build outside git, with
// -buildvcs=false, or a `go install module@version`.
type buildVCS struct {
	Revision string
	Modified bool
	Time     string // the commit's time (vcs.time), not the build's
}

func vcsOf(info *debug.BuildInfo) buildVCS {
	var b buildVCS
	if info == nil {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		case "vcs.time":
			b.Time = s.Value
		}
	}
	return b
}

// runningBuild reads this binary's build info. Tests replace it.
var runningBuild = func() buildVCS {
	info, _ := debug.ReadBuildInfo()
	return vcsOf(info)
}

// bopsModule is the module path that marks a checkout as bops's own source.
const bopsModule = "github.com/extremenelson/beads-ops/bops"

// staleBinary returns a warning when root is a checkout of bops's own source
// and this binary was built before the last commit there that changed bops/,
// so a command run now is not the code on the checkout (bops-y8x). It is ""
// in any other repository, when the binary is current, and when git cannot
// say: doctor's lock checks must not depend on it.
func staleBinary(ctx context.Context, root string, b buildVCS) string {
	if modulePath(filepath.Join(root, "bops", "go.mod")) != bopsModule {
		return ""
	}
	rebuild := fmt.Sprintf("rebuild it: (cd %s && go install ./cmd/bops)", filepath.Join(root, "bops"))
	last, err := gitOut(ctx, root, "log", "-1", "--format=%H", "--", "bops/")
	if err != nil || last == "" {
		return ""
	}
	if b.Revision == "" {
		return "this bops records no commit (built outside git, with -buildvcs=false, or by go install with a version), so doctor cannot tell whether it predates bops/ here; " + rebuild
	}
	if b.Revision == last {
		return ""
	}
	err = exec.CommandContext(ctx, "git", "-C", root, "merge-base", "--is-ancestor", last, b.Revision).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return "" // built from a commit that already has the last bops/ change
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return fmt.Sprintf("this bops was built from %s, before %s, the last commit to change bops/ here: commands run the older code; %s", short(b.Revision), short(last), rebuild)
	}
	return fmt.Sprintf("this bops was built from %s, which this checkout does not have, so doctor cannot tell whether it predates bops/ here (last change %s); fetch, or %s", short(b.Revision), short(last), rebuild)
}

// modulePath is the module line of a go.mod, or "" when there is none.
func modulePath(goMod string) string {
	f, err := os.Open(goMod)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
