//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeConfigWaitPollInterval = 500 * time.Millisecond
	wakeConfigUnreportedGrace  = 5 * time.Second
)

type wakeConfigSettingJSON struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

type wakeConfigWakeJSON struct {
	Status     string `json:"status"`
	Generation string `json:"generation"`
	Error      string `json:"error"`
}

type wakeConfigFileJSON struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

type wakeConfigJSON struct {
	Schema      int                              `json:"schema"`
	Agent       string                           `json:"agent"`
	Root        string                           `json:"root"`
	Settings    map[string]wakeConfigSettingJSON `json:"settings"`
	RestartOnly []string                         `json:"restart_only"`
	File        wakeConfigFileJSON               `json:"file"`
	Wake        wakeConfigWakeJSON               `json:"wake"`
}

// refuseWakeRestartOnlyFlags rejects the flags that are bound into a running
// wake's identity or transport. They are checked before parsing so the
// operator gets the restart instruction instead of "flag provided but not
// defined".
func refuseWakeRestartOnlyFlags(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			return nil
		}
		name := strings.TrimLeft(arg, "-")
		if name == arg || name == "" {
			continue
		}
		name, _, _ = strings.Cut(name, "=")
		for _, restartOnly := range wakeRestartOnlyFlags {
			if name == restartOnly {
				return UsageError(
					"--%s is fixed for a running wake; restart the wake through its owning terminal or supervisor",
					restartOnly,
				)
			}
		}
	}
	return nil
}

func runWakeConfig(args []string) error {
	if err := refuseWakeRestartOnlyFlags(args); err != nil {
		return err
	}
	fs := flag.NewFlagSet("wake config", flag.ContinueOnError)
	common := addCommonFlags(fs)
	values := registerWakeSettingsFlags(fs)
	var unsetKeys multiStringFlag
	fs.Var(&unsetKeys, "unset", "Return a setting to its default by key, e.g. hold_normal (repeatable)")
	reset := fs.Bool("reset", false, "Replace the settings file with exactly the given setting flags (none = all defaults)")
	wait := fs.Bool("wait", false, "Wait until the running wake has applied the settings file")
	timeout := fs.Duration("timeout", 60*time.Second, "Maximum time for --wait (0 = wait forever)")

	usage := usageWithFlags(fs, "amq wake config [--me <agent>] [setting flags] [--unset <key>]... [--reset] [--wait] [options]",
		"Show or change the live settings of the agent's wake.",
		"",
		"With no setting flag and no --unset, print each setting, its value, and whether it",
		"comes from the file or the default, then what the running wake did with the file.",
		"Otherwise validate the whole new set and store it in .wake.settings. A running wake",
		"applies the file within a few seconds. Flags fixed for a running wake (--inject-via,",
		"--inject-mode, --inject-arg, --inject-cmd, --interrupt-cmd, --retry-until) are refused.",
		"An invalid file is shown as refused and cannot be changed with set or --unset;",
		"--reset replaces it with exactly the given setting flags (none = all defaults).")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	if *timeout < 0 {
		return UsageError("--timeout must be >= 0")
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	explicit := visitedWakeSettingsKeys(fs)
	if *reset && len(unsetKeys) > 0 {
		return UsageError("--reset cannot be combined with --unset")
	}
	for _, key := range unsetKeys {
		if _, ok := lookupWakeSettingDef(key); !ok {
			return UsageError(
				"--unset: unknown wake setting %q (valid: %s)",
				key,
				strings.Join(wakeSettingsKeys(), ", "),
			)
		}
	}
	agentDir, err := openExistingWakeAgentDir(root, me)
	if err != nil {
		// A missing mailbox or bad handle keeps its own exit code; the wake
		// diagnostic wrap would turn it into a general error.
		var exitErr *ExitCodeError
		if errors.As(err, &exitErr) {
			return err
		}
		return withWakeDiagnostic(err, canonicalWakeRoot(root), me)
	}
	defer func() { _ = agentDir.Close() }()

	var raw []byte
	var exists bool
	switch {
	case *reset:
		raw, err = resetWakeSettingsFileInDir(agentDir, *values, explicit)
		if err != nil {
			return err
		}
		exists = true
	case len(explicit) > 0 || len(unsetKeys) > 0:
		if err := requireValidWakeSettingsFile(agentDir); err != nil {
			return err
		}
		raw, _, err = updateWakeSettingsFileInDir(agentDir, func(doc *wakeSettingsDoc) error {
			for _, key := range unsetKeys {
				if err := doc.unset(key); err != nil {
					return UsageError("--unset: %v", err)
				}
			}
			doc.set(*values, explicit)
			return nil
		})
		if err != nil {
			// The lifecycle guard joins its unlock result onto fn's error,
			// which hides the usage exit code of an invalid merged value.
			var exitErr *ExitCodeError
			if errors.As(err, &exitErr) {
				return exitErr
			}
			return err
		}
		exists = true
	default:
		raw, exists, err = readWakeSettingsFile(agentDir)
	}

	// A file that cannot be read or decoded is shown, not hidden: the
	// effective values are the defaults and the file status carries the error.
	file := wakeConfigFileJSON{Status: "ok"}
	if !exists {
		file.Status = "absent"
	}
	var doc wakeSettingsDoc
	var effective wakeSettings
	if err == nil && exists {
		doc, err = decodeWakeSettingsDoc(raw)
	}
	if err == nil {
		effective, err = doc.effective()
	}
	if err != nil {
		doc = wakeSettingsDoc{}
		effective = defaultWakeSettings()
		file = wakeConfigFileJSON{Status: "refused", Error: err.Error()}
	}
	digest := wakeSettingsDigest(raw, exists)
	canonicalRoot := canonicalWakeRoot(root)

	state, err := inspectWakeSettingsRunState(agentDir, canonicalRoot, me, digest)
	if err != nil {
		return err
	}
	var waitErr error
	if *wait && state.Status != wakeSettingsRunNone {
		state, waitErr = waitWakeSettingsApplied(agentDir, canonicalRoot, me, digest, *timeout)
	}
	if err := printWakeConfig(common.JSON, me, canonicalRoot, file, doc, effective, state); err != nil {
		return err
	}
	return waitErr
}

// waitWakeSettingsApplied polls until the running wake reports the file
// digest applied or refused, or no wake runs. The generation is read from
// the lock on every poll, so a wake replaced during the wait is followed.
func waitWakeSettingsApplied(
	agentDir *wakeAgentDir,
	root, me, digest string,
	timeout time.Duration,
) (wakeSettingsRunState, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	var unreportedSince time.Time
	for {
		state, err := inspectWakeSettingsRunState(agentDir, root, me, digest)
		if err != nil {
			return state, err
		}
		switch state.Status {
		case wakeSettingsRunApplied, wakeSettingsRunNone:
			return state, nil
		case wakeSettingsRunRefused:
			return state, fmt.Errorf("wake refused the settings file: %s", state.Error)
		case wakeSettingsRunUnreported:
			// A starting wake records its first status just after it takes
			// the lock, and a failed status write retries on the next tick.
			// Only a wake that stays silent past that window is an older image.
			if unreportedSince.IsZero() {
				unreportedSince = time.Now()
			} else if time.Since(unreportedSince) >= wakeConfigUnreportedGrace {
				return state, ActionRequiredError("%s", wakeConfigUnreportedText)
			}
		default:
			unreportedSince = time.Time{}
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return state, TimeoutError("wake config --wait timed out after %s; the running wake has not applied the settings", timeout)
		}
		time.Sleep(wakeConfigWaitPollInterval)
	}
}

const wakeConfigUnreportedText = "running wake does not report live settings (an older image, or its status write failed); restart it to use live settings"

// openExistingWakeAgentDir opens agents/<me> without creating it, so a typo
// handle is a not-found error and not a new mailbox directory.
func openExistingWakeAgentDir(root, me string) (*wakeAgentDir, error) {
	if err := fsq.ValidateHandle(me); err != nil {
		return nil, UsageError("--me: %v", err)
	}
	path := fsq.AgentBase(root, me)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, NotFoundError(
				"mailbox for %q not found at %s; check the handle or run amq init",
				me, path,
			)
		}
		return nil, fmt.Errorf("stat wake agent directory %s: %w", path, err)
	}
	return openWakeAgentDir(root, me)
}

// requireValidWakeSettingsFile refuses set and --unset on a file that cannot
// be decoded, with the way out.
func requireValidWakeSettingsFile(agentDir *wakeAgentDir) error {
	raw, exists, err := readWakeSettingsFile(agentDir)
	if err == nil && exists {
		var doc wakeSettingsDoc
		if doc, err = decodeWakeSettingsDoc(raw); err == nil {
			_, err = doc.effective()
		}
	}
	if err != nil {
		return fmt.Errorf(
			"the wake settings file is invalid (%v); fix it by hand (mode 0600) or run 'amq wake config --reset [settings flags]'",
			err,
		)
	}
	return nil
}

// resetWakeSettingsFileInDir replaces the file with exactly the given keys,
// validated before anything is written, and returns the stored bytes.
func resetWakeSettingsFileInDir(agentDir *wakeAgentDir, values wakeSettings, keys []string) ([]byte, error) {
	var doc wakeSettingsDoc
	doc.set(values, keys)
	if _, err := doc.effective(); err != nil {
		return nil, UsageError("%v", err)
	}
	stored, err := doc.encode()
	if err != nil {
		return nil, err
	}
	err = withWakeLifecycleGuardModeAndTimeoutInDir(
		agentDir,
		unix.LOCK_EX|unix.LOCK_NB,
		wakeSettingsGuardTimeout,
		func(dirfd int) error { return writeWakeSettingsFileAt(dirfd, agentDir, stored) },
	)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

func formatWakeSettingValue(def wakeSettingDef, settings wakeSettings) any {
	switch p := def.field(&settings).(type) {
	case *time.Duration:
		return p.String()
	case *int:
		return *p
	case *bool:
		return *p
	case *string:
		return *p
	}
	return nil
}

func printWakeConfig(
	asJSON bool,
	me, root string,
	file wakeConfigFileJSON,
	doc wakeSettingsDoc,
	effective wakeSettings,
	state wakeSettingsRunState,
) error {
	source := func(key string) string {
		if doc.present[key] {
			return "file"
		}
		return "default"
	}
	if asJSON {
		out := wakeConfigJSON{
			Schema:      1,
			Agent:       me,
			Root:        root,
			Settings:    make(map[string]wakeConfigSettingJSON, len(wakeSettingDefs)),
			RestartOnly: wakeRestartOnlyFlags,
			File:        file,
			Wake:        wakeConfigWakeJSON(state),
		}
		for _, def := range wakeSettingDefs {
			out.Settings[def.key] = wakeConfigSettingJSON{
				Value:  formatWakeSettingValue(def, effective),
				Source: source(def.key),
			}
		}
		return writeJSON(os.Stdout, out)
	}
	fileLine := "file: " + file.Status
	if file.Status == "refused" {
		fileLine += ": " + file.Error
	}
	if err := writeStdoutLine(fileLine); err != nil {
		return err
	}
	for _, def := range wakeSettingDefs {
		if err := writeStdout("%-20s %v (%s)\n", def.key, formatWakeSettingValue(def, effective), source(def.key)); err != nil {
			return err
		}
	}
	line := "wake: " + state.Status
	switch state.Status {
	case wakeSettingsRunNone:
		line = "wake: no running wake"
	case wakeSettingsRunRefused:
		line = "wake: refused: " + state.Error
	case wakeSettingsRunUnreported:
		line = "wake: " + wakeConfigUnreportedText
	}
	return writeStdoutLine(line)
}
