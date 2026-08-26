//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const coopNamedTUIStartupDelay = 3 * time.Second

var (
	openCoopNamedTTY = func() (*os.File, error) {
		return os.OpenFile("/dev/tty", os.O_RDWR, 0)
	}
	coopNamedTTYReady = func() bool {
		return tiocsti.Available() && coopNamedHasControllingTTY()
	}
	coopNamedTTYInject = func(me, binaryBase string) error {
		return injectCoopNamedSlashCommand(me, binaryBase)
	}
	coopNamedStartupSleep = func(time.Duration) { time.Sleep(coopNamedTUIStartupDelay) }
)

var startCoopNamedTUIInjector = func(me, cmdName string) error {
	return startCoopNamedTUIInjectorProcess(me, cmdName)
}

func startCoopNamedTUIInjectorProcess(me, cmdName string) error {
	amqExecutable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve amq executable for coop named inject: %w", err)
	}
	amqBin, err := coopWakeExecutionPath(amqExecutable)
	if err != nil {
		return err
	}
	cmd := exec.Command(
		amqBin,
		"--no-update-check",
		"coop",
		"named-inject",
		"--me", me,
		"--binary", filepath.Base(cmdName),
	)
	// Leave stdin/stdout on the null device so the helper does not steal the
	// agent's TTY fds. Injection opens the controlling terminal via /dev/tty.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start coop named inject helper: %w", err)
	}
	// Detach from Wait so the helper is not a tracked child of the agent after exec.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release coop named inject helper: %w", err)
	}
	return nil
}

func coopNamedHasControllingTTY() bool {
	tty, err := openCoopNamedTTY()
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

func runCoopNamedInject(args []string) error {
	fs := flag.NewFlagSet("coop named-inject", flag.ContinueOnError)
	meFlag := fs.String("me", "", "AMQ handle to stamp onto the CLI session")
	binaryFlag := fs.String("binary", "", "Spawned CLI binary basename")
	delayFlag := fs.Duration("startup-delay", coopNamedTUIStartupDelay, "Delay before terminal injection")
	usage := func() {
		_ = writeStderr("usage: amq coop named-inject --me <handle> --binary <basename>\n")
	}
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	me := strings.TrimSpace(*meFlag)
	if me == "" {
		return UsageError("--me is required")
	}
	if _, err := normalizeHandle(me); err != nil {
		return UsageError("--me: %v", err)
	}
	binaryBase := strings.TrimSpace(*binaryFlag)
	if binaryBase == "" {
		return UsageError("--binary is required")
	}
	if coopNamedModeFor(binaryBase) != coopNamedModeTUI {
		return UsageError("--binary %q does not use coop named terminal injection", binaryBase)
	}
	if *delayFlag > 0 {
		coopNamedStartupSleep(*delayFlag)
	}
	if err := coopNamedTTYInject(me, binaryBase); err != nil {
		_ = writeStderr("warning: coop named inject failed: %v\n", err)
	}
	return nil
}

func injectCoopNamedSlashCommand(me, binaryBase string) error {
	if !coopNamedTTYReady() {
		if !tiocsti.Available() {
			return errors.New("TIOCSTI terminal injection is unavailable")
		}
		return errors.New("no controlling terminal for coop named inject")
	}
	command := coopNamedTUICommand(binaryBase, me)
	for _, ch := range command {
		if ch < 0x20 || ch > 0x7e {
			return fmt.Errorf("coop named command contains non-printable ASCII")
		}
	}
	if err := tiocstiInject(command); err != nil {
		return err
	}
	// One Enter to submit the slash command. Extra newlines can land in the
	// composer as a blank user message if the TUI is already at a prompt.
	rawInjectSleep(rawInjectSettleDelay)
	return tiocstiInject("\r")
}
