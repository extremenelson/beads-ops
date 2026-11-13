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

// waitDelay bounds how long Run waits, after the deadline kills git, for git's
// output pipes to close. git's ssh child inherits them and can outlive git by
// minutes (ServerAliveInterval*ServerAliveCountMax), so without this a call cut
// off at callTimeout could still block long after it.
const waitDelay = 2 * time.Second

// sshConnectTimeout keeps a silently dropped connect from using the whole call.
const sshConnectTimeout = "10"

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
	cmd.Env = append(os.Environ(), e.env(ctx, c)...)
	cmd.Stdin = strings.NewReader(c.Stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	return Output{Stdout: out.String(), Stderr: errb.String()}, err
}

// env fixes the commit identity, so user.email is never published and a machine
// with no git identity still works, and keeps git from prompting.
func (e Exec) env(ctx context.Context, c Cmd) []string {
	vars := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=bops", "GIT_AUTHOR_EMAIL=bops@invalid",
		"GIT_COMMITTER_NAME=bops", "GIT_COMMITTER_EMAIL=bops@invalid",
	}
	if len(c.Args) == 0 || (c.Args[0] != "push" && c.Args[0] != "fetch" && c.Args[0] != "ls-remote") {
		return vars // only these reach the network
	}
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
	cmd.Dir = e.Dir
	configured, _ := cmd.Output() // exits non-zero when unset
	if ssh := sshCommand(os.Getenv, strings.TrimSpace(string(configured))); ssh != "" {
		vars = append(vars, "GIT_SSH_COMMAND="+ssh)
	}
	return vars
}

// sshCommand adds BatchMode and ConnectTimeout to the command git would run,
// because setting GIT_SSH_COMMAND outright replaces core.sshCommand (probed). An
// option already present is left as the user set it. A wrapper that is not ssh
// may not accept the options, so it is left alone.
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
	if len(fields) == 0 {
		return base
	}
	program := fields[0][strings.LastIndexAny(fields[0], `/\`)+1:]
	if program != "ssh" && program != "ssh.exe" {
		return base
	}
	if !strings.Contains(base, "BatchMode") {
		base += " -o BatchMode=yes"
	}
	if !strings.Contains(base, "ConnectTimeout") {
		base += " -o ConnectTimeout=" + sshConnectTimeout
	}
	return base
}
