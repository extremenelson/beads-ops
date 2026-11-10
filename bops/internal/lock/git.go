package lock

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// callTimeout bounds one git call. A push cut off here is settled by re-reading
// the ref, so a timeout cannot produce a wrong answer.
const callTimeout = 20 * time.Second

// Cmd is one git invocation.
type Cmd struct {
	Args  []string
	Stdin string
}

// Output is what git wrote. It is meaningful even when Run returns an error.
type Output struct {
	Stdout string
	Stderr string
}

// Git runs git. It is the seam tests use to inject failures.
type Git interface {
	Run(ctx context.Context, c Cmd) (Output, error)
}

// Exec runs the real git in Dir.
type Exec struct {
	Dir     string
	Timeout time.Duration // defaults to callTimeout
}

// Run implements Git.
func (e Exec) Run(ctx context.Context, c Cmd) (Output, error) {
	timeout := e.Timeout
	if timeout == 0 {
		timeout = callTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", c.Args...)
	cmd.Dir = e.Dir
	cmd.Env = append(os.Environ(), e.env(ctx)...)
	cmd.Stdin = strings.NewReader(c.Stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return Output{Stdout: out.String(), Stderr: errb.String()}, err
}

// env fixes the commit identity, so user.email is never published and a machine
// with no git identity still works, and keeps git from prompting.
func (e Exec) env(ctx context.Context) []string {
	vars := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=bops", "GIT_AUTHOR_EMAIL=bops@invalid",
		"GIT_COMMITTER_NAME=bops", "GIT_COMMITTER_EMAIL=bops@invalid",
	}
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
	cmd.Dir = e.Dir
	configured, _ := cmd.Output() // exits non-zero when unset
	if ssh := sshCommand(os.Getenv, strings.TrimSpace(string(configured))); ssh != "" {
		vars = append(vars, "GIT_SSH_COMMAND="+ssh)
	}
	return vars
}

// sshCommand adds BatchMode to the command git would run, because setting
// GIT_SSH_COMMAND outright replaces core.sshCommand (probed). A wrapper that
// is not ssh may not accept the option, so it is left alone.
func sshCommand(getenv func(string) string, configured string) string {
	base := getenv("GIT_SSH_COMMAND")
	if base == "" && getenv("GIT_SSH") != "" {
		return "" // a bare program path, which git treats differently
	}
	if base == "" {
		base = configured
	}
	if base == "" {
		base = "ssh"
	}
	fields := strings.Fields(base)
	if len(fields) == 0 || strings.Contains(base, "BatchMode") {
		return base
	}
	program := fields[0][strings.LastIndexAny(fields[0], `/\`)+1:]
	if program != "ssh" && program != "ssh.exe" {
		return base
	}
	return base + " -o BatchMode=yes"
}
