// Package buzzcli runs the buzz CLI as the Buzz managed agent and reads what
// a CLI write returned.
package buzzcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnvCLI overrides the buzz binary used to post.
const EnvCLI = "AMQ_ACP_BUZZ_CLI"

// Port runs the buzz CLI as the managed agent. A nil *Port has no identity.
//
// Invariants:
//   - The agent's private key reaches only the child process environment,
//     never argv, a log line or an error text.
//   - The buzz binary is found without a PATH fallback: EnvCLI, else only a
//     file named exactly "buzz" inside Buzz.app.
//   - The child gets a minimal environment: PATH, HOME and the identity.
type Port struct {
	// env holds "KEY=value" entries of the agent's credentials.
	env []string
}

// New returns a port whose child environment carries env, "KEY=value"
// entries of the agent's credentials.
func New(env []string) *Port { return &Port{env: env} }

// HasIdentity reports whether the port has credentials to run as.
func (p *Port) HasIdentity() bool { return p != nil && len(p.env) > 0 }

// ErrNoIdentity means this process runs outside a Buzz managed agent, so
// there is nothing to post as; the ACP text is the only channel.
var ErrNoIdentity = errors.New("no Buzz agent identity")

// NotSentError marks a post that failed before the buzz CLI started, so
// nothing can have reached the relay and the post may be made again.
type NotSentError struct{ Err error }

func (e *NotSentError) Error() string { return e.Err.Error() }
func (e *NotSentError) Unwrap() error { return e.Err }

// Run runs the buzz CLI with only the agent identity and a minimal
// environment and returns its stdout.
func (p *Port) Run(budget time.Duration, stdin io.Reader, args ...string) ([]byte, error) {
	return p.RunContext(context.Background(), budget, stdin, args...)
}

// RunContext is Run that also stops when parent ends. A run that never
// started is a NotSentError: it cannot have reached the relay.
func (p *Port) RunContext(parent context.Context, budget time.Duration, stdin io.Reader, args ...string) ([]byte, error) {
	return p.RunBounded(parent, budget, stdin, 0, args...)
}

// ErrOutputLimit is a buzz CLI run stopped because its stdout passed the
// caller's limit.
var ErrOutputLimit = errors.New("buzz CLI output is over the limit")

// boundedBuffer holds at most limit bytes. The write that would pass limit
// fails and calls stop, which kills the CLI, so the output never grows past
// limit however much the CLI writes. It is not an io.ReaderFrom, so every
// copy goes through Write.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
	stop  func()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.over = true
		b.stop()
		return 0, ErrOutputLimit
	}
	return b.buf.Write(p)
}

// RunBounded is RunContext whose stdout is limited to stdoutMax bytes when
// stdoutMax > 0: a run that writes more is killed and fails with
// ErrOutputLimit.
func (p *Port) RunBounded(parent context.Context, budget time.Duration, stdin io.Reader, stdoutMax int, args ...string) ([]byte, error) {
	if !p.HasIdentity() {
		return nil, ErrNoIdentity
	}
	bin, err := buzzCLI()
	if err != nil {
		var notSent *NotSentError
		if !errors.As(err, &notSent) {
			err = &NotSentError{err}
		}
		return nil, err
	}
	// The budget covers the run and the pipe drain: a CLI whose child keeps
	// stderr open must not hold a status post (and its event post lock)
	// past the budget. A CLI that keeps sending after its budget is outside
	// the status/final ordering (README).
	waitDelay := budget / 10
	ctx, cancel := context.WithTimeout(parent, budget-waitDelay)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, p.env...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	stdout := &boundedBuffer{limit: stdoutMax, stop: cancel}
	if stdoutMax > 0 {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &stdout.buf
	}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, &NotSentError{fmt.Errorf("buzz %s: %w", strings.Join(args[:2], " "), err)}
	}
	if err := cmd.Wait(); err != nil {
		if stdout.over {
			return nil, fmt.Errorf("buzz %s: %w", strings.Join(args[:2], " "), ErrOutputLimit)
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			code = exit.ExitCode()
		}
		return nil, &ExitError{Code: code, Err: fmt.Errorf("buzz %s: %v: %s", strings.Join(args[:2], " "), err, msg)}
	}
	return stdout.buf.Bytes(), nil
}

// ExitError is a buzz CLI run that started and failed. Code is the CLI's
// exit status: 1 bad input, 2 relay or network, 3 auth, 4 other, 5 write
// conflict; -1 when the run was cut off (a timeout or a signal).
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// buzzCLI finds the buzz binary that receives the managed agent's key:
// AMQ_ACP_BUZZ_CLI, else the CLI inside Buzz.app under /Applications or the
// user's Applications.
// There is no PATH fallback: an unrelated buzz on PATH must never receive
// BUZZ_PRIVATE_KEY.
func buzzCLI() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvCLI)); p != "" {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	return buzzCLIFrom("/Applications", home)
}

// buzzCLIFrom selects the first usable Buzz.app CLI: systemApps first, then
// home/Applications. A home that is not absolute yields no candidate.
func buzzCLIFrom(systemApps, home string) (string, error) {
	dirs := []string{systemApps}
	if filepath.IsAbs(home) {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	for _, d := range dirs {
		if p := bundledBuzz(filepath.Join(d, "Buzz.app")); p != "" {
			return p, nil
		}
	}
	return "", &NotSentError{errors.New("buzz CLI not found in Buzz.app; set " + EnvCLI + " to the buzz executable")}
}

// bundledBuzz returns the CLI inside one Buzz.app, or "" unless the whole path
// from the filesystem root down contains no symlink (so an ancestor such as a
// symlinked home or Applications cannot redirect the key to another tree), the
// CLI is a regular file, and this process can execute it. The installation and
// its parent directories are trusted against concurrent replacement: pathname
// validation is not atomic with exec.
//
// The CLI file is named exactly "buzz" (byte for byte). A file whose name
// differs only in case (for example the Buzz Desktop GUI binary "Buzz") is
// another program and never gets the key, so the entry is looked up in the
// directory listing, not by a case-insensitive path resolution.
func bundledBuzz(app string) string {
	macos := filepath.Join(app, "Contents", "MacOS")
	entries, err := os.ReadDir(macos)
	if err != nil {
		return ""
	}
	found := false
	for _, e := range entries {
		if e.Name() == "buzz" {
			found = true
			break
		}
	}
	if !found {
		return ""
	}
	path := filepath.Join(macos, "buzz")
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != filepath.Clean(path) {
		return ""
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || !canExecute(path) {
		return ""
	}
	return path
}
