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

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const coopNamedTUIStartupDelay = 3 * time.Second

var (
	coopNamedTTYReady = func() bool {
		return tiocsti.Available() && coopNamedHasControllingTTY()
	}
	coopNamedTTYInject = func(me, binaryBase string, inputFree func() bool) error {
		return injectCoopNamedSlashCommand(me, binaryBase, inputFree)
	}
)

func openCoopNamedTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func coopNamedStartupSleep(delay time.Duration) { time.Sleep(delay) }

var startCoopNamedTUIInjector = func(name, cmdName, root, handle string, execStart time.Time) error {
	return startCoopNamedTUIInjectorProcess(name, cmdName, root, handle, execStart)
}

func startCoopNamedTUIInjectorProcess(name, cmdName, root, handle string, execStart time.Time) error {
	amqExecutable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve amq executable for coop named inject: %w", err)
	}
	amqBin, err := coopWakeExecutionPath(amqExecutable)
	if err != nil {
		return err
	}
	args := []string{
		amqBin,
		"--no-update-check",
		"coop",
		"named-inject",
		"--name", name,
		"--binary", cmdName,
		"--exec-start-ns", strconv.FormatInt(execStart.UnixNano(), 10),
		"--root", root,
		"--me", handle,
	}
	if launch.ProviderForExecutable(cmdName) == launch.CodexProvider {
		target, err := captureCodexNamingTarget(cmdName)
		if err != nil {
			return err
		}
		args = append(args,
			"--parent-pid", strconv.Itoa(target.PID),
			"--parent-process-start", target.ProcessStart,
			"--parent-boot-id", target.BootID,
			"--parent-executable", target.InitialExecutable,
			"--parent-executable-device", strconv.FormatUint(target.InitialIdentity.Device, 10),
			"--parent-executable-inode", strconv.FormatUint(target.InitialIdentity.Inode, 10),
			"--provider-binary", target.ProviderBinary,
			"--provider-binary-device", strconv.FormatUint(target.ProviderIdentity.Device, 10),
			"--provider-binary-inode", strconv.FormatUint(target.ProviderIdentity.Inode, 10),
		)
	}
	cmd := exec.Command(args[0], args[1:]...)
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
	meFlag := fs.String("me", "", "Agent handle whose dialog marker holds the typed command")
	rootFlag := fs.String("root", "", "Queue root of that agent")
	binaryFlag := fs.String("binary", "", "Spawned CLI binary basename")
	delayFlag := fs.Duration("startup-delay", coopNamedTUIStartupDelay, "Delay before terminal injection")
	execStartFlag := fs.Int64("exec-start-ns", 0, "Internal coop exec start timestamp")
	parentPIDFlag := fs.Int("parent-pid", os.Getppid(), "Internal spawned CLI PID")
	parentProcessStartFlag := fs.String("parent-process-start", "", "Internal spawned CLI process start token")
	parentBootIDFlag := fs.String("parent-boot-id", "", "Internal spawned CLI boot identity")
	parentExecutableFlag := fs.String("parent-executable", "", "Internal pre-exec AMQ executable")
	parentExecutableDeviceFlag := fs.Uint64("parent-executable-device", 0, "Internal pre-exec AMQ executable device")
	parentExecutableInodeFlag := fs.Uint64("parent-executable-inode", 0, "Internal pre-exec AMQ executable inode")
	providerBinaryFlag := fs.String("provider-binary", "", "Internal resolved provider executable")
	providerBinaryDeviceFlag := fs.Uint64("provider-binary-device", 0, "Internal provider executable device")
	providerBinaryInodeFlag := fs.Uint64("provider-binary-inode", 0, "Internal provider executable inode")
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
		return UsageError("--name is required")
	}
	if err := validateCoopNamedSessionLabel(name); err != nil {
		return UsageError("--name: %v", err)
	}
	binaryPath := strings.TrimSpace(*binaryFlag)
	if binaryPath == "" {
		return UsageError("--binary is required")
	}
	binaryBase := filepath.Base(binaryPath)
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
	if launch.ProviderForExecutable(binaryBase) == launch.CodexProvider {
		target := codexNamingTarget{
			PID:               *parentPIDFlag,
			ProcessStart:      strings.TrimSpace(*parentProcessStartFlag),
			BootID:            strings.TrimSpace(*parentBootIDFlag),
			InitialExecutable: strings.TrimSpace(*parentExecutableFlag),
			InitialIdentity:   codexFileIdentity{Device: *parentExecutableDeviceFlag, Inode: *parentExecutableInodeFlag},
			ProviderBinary:    strings.TrimSpace(*providerBinaryFlag),
			ProviderIdentity:  codexFileIdentity{Device: *providerBinaryDeviceFlag, Inode: *providerBinaryInodeFlag},
		}
		if target.PID <= 0 || target.ProcessStart == "" || target.BootID == "" ||
			target.InitialExecutable == "" || !filepath.IsAbs(target.InitialExecutable) || filepath.Clean(target.InitialExecutable) != target.InitialExecutable ||
			target.InitialIdentity.Device == 0 || target.InitialIdentity.Inode == 0 ||
			target.ProviderBinary == "" || !filepath.IsAbs(target.ProviderBinary) || filepath.Clean(target.ProviderBinary) != target.ProviderBinary ||
			target.ProviderIdentity.Device == 0 || target.ProviderIdentity.Inode == 0 {
			return UsageError("Codex named-inject requires complete parent process identity and provider binary")
		}
		if err := runCodexNamedSidecar(name, target); err != nil {
			_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, err.Error()))
		}
		return nil
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
	// The typed command and its Enter would answer an open dialog, so it
	// waits for the dialog marker exactly as the doorbell does.
	root, handle := strings.TrimSpace(*rootFlag), strings.TrimSpace(*meFlag)
	facts, _ := harness.ForExecutable(binaryBase)
	if !waitCoopNamedInputFree(root, handle, facts.Name, wakeDialogQuietHold, wakeDialogRecheck) {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, "a harness dialog stayed open"))
		return nil
	}
	inputFree := func() bool { return waitCoopNamedInputFree(root, handle, facts.Name, 0, 0) }
	if err := runCoopNamedTUI(reader, name, binaryBase, cwd, execStart, inputFree); err != nil {
		_ = writeStderr("warning: coop named inject: %v\n", err)
	}
	return nil
}

// waitCoopNamedInputFree waits until the agent's dialog marker no longer
// holds input, reading it as the wake of harnessName's dialog source does,
// checking every interval for at most limit. It reports false when the
// marker still holds at the limit, or when the agent's directory cannot be
// read.
func waitCoopNamedInputFree(root, handle, harnessName string, limit, interval time.Duration) bool {
	if root == "" || handle == "" {
		return false
	}
	agentDir, err := openWakeAgentDir(root, handle)
	if err != nil {
		return false
	}
	defer func() { _ = agentDir.Close() }()
	source := wakeDialogSourceInDir(agentDir, harnessName)
	deadline := time.Now().Add(limit)
	for {
		if state, err := source(time.Now()); err == nil && state == wakeDialogClosed {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		coopNamedStartupSleep(min(interval, time.Until(deadline)))
	}
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

func runCoopNamedTUI(reader coopNamedStoreReader, name, binaryBase, cwd string, execStart time.Time, inputFree func() bool) error {
	candidate, err := reader.locate(cwd, execStart)
	if err != nil {
		_ = writeStderr("%s\n", coopNamedTUIManualReminder(name, binaryBase, err.Error()))
		return nil
	}
	if strings.TrimSpace(candidate.name) != "" {
		return nil
	}
	if err := coopNamedTTYInject(name, binaryBase, inputFree); err != nil {
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

// errCoopNamedDialogOpen stops a naming attempt when a dialog marker
// appears: the typed text and Enter would answer the dialog.
var errCoopNamedDialogOpen = errors.New("a harness dialog opened")

// injectCoopNamedSlashCommand types the naming command and Enter. inputFree
// is checked before the command's write and again right before Enter, after
// the settle delay; a dialog marker then stops the attempt.
func injectCoopNamedSlashCommand(me, binaryBase string, inputFree func() bool) error {
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
	if inputFree != nil && !inputFree() {
		return errCoopNamedDialogOpen
	}
	if err := tiocstiInject(command); err != nil {
		return err
	}
	// One Enter to submit the slash command. Extra newlines can land in the
	// composer as a blank user message if the TUI is already at a prompt.
	// A harness with a paste submit delay needs that much quiet before it.
	delay := rawInjectSettleDelay
	if facts, _ := harness.ForExecutable(binaryBase); facts.PasteSubmitDelay > delay {
		delay = facts.PasteSubmitDelay
	}
	rawInjectSleep(delay)
	if inputFree != nil && !inputFree() {
		return errCoopNamedDialogOpen
	}
	return tiocstiInject("\r")
}
