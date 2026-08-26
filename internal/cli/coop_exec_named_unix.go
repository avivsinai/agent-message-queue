//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	coopNamedStartupSleep = func(delay time.Duration) { time.Sleep(delay) }
)

var startCoopNamedTUIInjector = func(name, cmdName string, execStart time.Time) error {
	return startCoopNamedTUIInjectorProcess(name, cmdName, execStart)
}

func startCoopNamedTUIInjectorProcess(name, cmdName string, execStart time.Time) error {
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
		"--name", name,
		"--binary", filepath.Base(cmdName),
		"--exec-start-ns", strconv.FormatInt(execStart.UnixNano(), 10),
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
	nameFlag := fs.String("name", "", "Session name to stamp onto the CLI session")
	meFlag := fs.String("me", "", "Legacy alias for --name")
	binaryFlag := fs.String("binary", "", "Spawned CLI binary basename")
	delayFlag := fs.Duration("startup-delay", coopNamedTUIStartupDelay, "Delay before terminal injection")
	execStartFlag := fs.Int64("exec-start-ns", 0, "Internal coop exec start timestamp")
	usage := func() {
		_ = writeStderr("usage: amq coop named-inject --name <session/name> --binary <basename>\n")
	}
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	name := strings.TrimSpace(*nameFlag)
	if name == "" {
		name = strings.TrimSpace(*meFlag)
	}
	if name == "" {
		return UsageError("--name is required")
	}
	if err := validateCoopNamedSessionLabel(name); err != nil {
		return UsageError("--name: %v", err)
	}
	binaryBase := strings.TrimSpace(*binaryFlag)
	if binaryBase == "" {
		return UsageError("--binary is required")
	}
	if coopNamedModeFor(binaryBase) != coopNamedModeTUI {
		return UsageError("--binary %q does not use coop named terminal injection", binaryBase)
	}
	execStart := time.Now()
	if *execStartFlag != 0 {
		execStart = time.Unix(0, *execStartFlag)
	}
	if *delayFlag > 0 {
		coopNamedStartupSleep(*delayFlag)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve coop named spawn cwd: %w", err)
	}
	reader, ok := coopNamedStoreReaderFor(binaryBase)
	if !ok {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, "unsupported store"))
		return nil
	}
	if err := runCoopNamedTUI(reader, name, binaryBase, cwd, execStart); err != nil {
		_ = writeStderr("warning: coop named inject: %v\n", err)
	}
	return nil
}

func validateCoopNamedSessionLabel(name string) error {
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 1:
		_, err := normalizeHandle(parts[0])
		return err
	case 2:
		if err := validateSessionName(parts[0]); err != nil {
			return err
		}
		_, err := normalizeHandle(parts[1])
		return err
	default:
		return fmt.Errorf("must be <handle> or <session>/<handle>")
	}
}

func coopNamedTUIManualReminder(name, binaryBase, reason string) string {
	return fmt.Sprintf(
		"warning: cannot confirm %s CLI session %q (%s); enter %q manually",
		filepath.Base(binaryBase), name, reason, coopNamedTUICommand(binaryBase, name),
	)
}

func runCoopNamedTUI(reader coopNamedStoreReader, name, binaryBase, cwd string, execStart time.Time) error {
	candidate, err := reader.locate(cwd, execStart)
	if err != nil {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, err.Error()))
		return nil
	}
	if strings.TrimSpace(candidate.name) != "" {
		return nil
	}
	if err := coopNamedTTYInject(name, binaryBase); err != nil {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, err.Error()))
		return nil
	}

	deadline := time.Now().Add(coopNamedTUIReadbackTimeout)
	for {
		actual, readErr := reader.readName(candidate)
		if readErr == nil && strings.TrimSpace(actual) == name {
			_ = writeStderr("named %s\n", name)
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		remaining := time.Until(deadline)
		if remaining > coopNamedTUIReadbackInterval {
			remaining = coopNamedTUIReadbackInterval
		}
		coopNamedReadbackSleep(remaining)
	}
	_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, "rename was not confirmed"))
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
