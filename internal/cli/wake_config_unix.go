//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// wakeConfigMachineRefused is the machine status a wake reports for a machine
// file it did not apply.
const wakeConfigMachineRefused = "refused"

const wakeConfigWaitPollInterval = 500 * time.Millisecond

// wakeConfigUnreportedGrace is a var only so a test can shorten it.
var wakeConfigUnreportedGrace = 5 * time.Second

type wakeConfigSettingJSON struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

type wakeConfigWakeJSON struct {
	Status       string `json:"status"`
	Generation   string `json:"generation"`
	Error        string `json:"error"`
	MachineState string `json:"machine_status,omitempty"`
	MachineError string `json:"machine_error,omitempty"`
}

type wakeConfigFileJSON struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// wakeConfigMachineFileJSON is the machine layer's file: ok, absent, or
// refused (unreadable, invalid, or invalid once merged under the agent file).
type wakeConfigMachineFileJSON struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Error  string `json:"error"`
	// merge marks a refusal that came from layering the machine file under
	// the agent file; the file is valid alone. Not part of the JSON.
	merge bool
}

// wakeConfigJSON keeps schema 1: machine mode without --me has no agent,
// root, agent file or wake block, so those are left out there.
type wakeConfigJSON struct {
	Schema      int                              `json:"schema"`
	Agent       string                           `json:"agent,omitempty"`
	Root        string                           `json:"root,omitempty"`
	Settings    map[string]wakeConfigSettingJSON `json:"settings"`
	RestartOnly []string                         `json:"restart_only"`
	File        *wakeConfigFileJSON              `json:"file,omitempty"`
	MachineFile wakeConfigMachineFileJSON        `json:"machine_file"`
	Wake        *wakeConfigWakeJSON              `json:"wake,omitempty"`
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
	c := &wakeConfigRun{}
	if err := refuseWakeRestartOnlyFlags(args); err != nil {
		return err
	}
	fs := flag.NewFlagSet("wake config", flag.ContinueOnError)
	c.common = addCommonFlags(fs)
	c.values = registerWakeSettingsFlags(fs)
	c.machine = fs.Bool("machine", false, "Show or change the machine settings (~/.amq/wake.settings) shared by every wake of this user")
	fs.Var(&c.unsetKeys, "unset", "Return a setting to the next layer (machine, then default) by key, e.g. hold_normal (repeatable)")
	c.reset = fs.Bool("reset", false, "Replace the settings file with exactly the given setting flags (none = no overrides)")
	c.wait = fs.Bool("wait", false, "Wait until the running wake has applied the settings files")
	c.timeout = fs.Duration("timeout", 60*time.Second, "Maximum time for --wait (0 = wait forever)")

	usage := usageWithFlags(fs, "amq wake config [--me <agent>] [--machine] [setting flags] [--unset <key>]... [--reset] [--wait] [options]",
		"Show or change the live settings of the agent's wake.",
		"",
		"Each setting comes from the agent's file (.wake.settings), else the machine file",
		"(~/.amq/wake.settings, shared by every wake of this user on this machine), else the",
		"built-in default. With no setting flag and no --unset, print each setting, its value,",
		"and which layer it comes from, then what the running wake did with the files.",
		"Otherwise validate the whole new set and store it. A running wake applies a change",
		"within a few seconds. Flags fixed for a running wake (--inject-via, --inject-mode,",
		"--inject-arg, --inject-cmd, --interrupt-cmd, --retry-until) are refused.",
		"An invalid file is shown as refused and cannot be changed with set or --unset;",
		"--reset replaces it with exactly the given setting flags. --reset with no setting",
		"flag means no overrides: the agent's file then falls through to the machine file and",
		"the defaults. --unset returns a key to the next layer (machine or default).",
		"",
		"With --machine, set, --unset and --reset change the machine file; --me is optional",
		"there and, when given, adds that agent's wake status. --machine --wait needs --me and",
		"waits on that one wake; other wakes are not tracked. --wait exits 1 when the wake",
		"refuses the file this command changed; a refusal of the other file is a warning",
		"and exit 0.",
		"",
		"While the agent file is absent and the running wake does not report live settings",
		"(unreported) past a short startup grace, set and --unset exit 6; the wake may be an older",
		"image, have a failed status write, or be a resume still storing its command-line settings",
		"in the file. Restart the wake or use --reset with the full set.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	hasMe := strings.TrimSpace(c.common.Me) != ""
	if !*c.machine {
		if err := requireMe(c.common.Me); err != nil {
			return err
		}
	}
	if *c.machine && *c.wait && !hasMe {
		return UsageError("--machine --wait needs --me (or AM_ME) to name the wake to wait on")
	}
	if hasMe {
		var err error
		c.me, err = normalizeHandle(c.common.Me)
		if err != nil {
			return UsageError("--me: %v", err)
		}
	}
	if *c.timeout < 0 {
		return UsageError("--timeout must be >= 0")
	}
	var root string
	if hasMe {
		root = resolveRoot(c.common.Root)
		if err := validateKnownHandles(root, c.common.Strict, c.me); err != nil {
			return err
		}
	}

	c.explicit = visitedWakeSettingsKeys(fs)
	if *c.reset && len(c.unsetKeys) > 0 {
		return UsageError("--reset cannot be combined with --unset")
	}
	for _, key := range c.unsetKeys {
		if _, ok := lookupWakeSettingDef(key); !ok {
			return UsageError(
				"--unset: unknown wake setting %q (valid: %s)",
				key,
				strings.Join(wakeSettingsKeys(), ", "),
			)
		}
	}
	c.changing = *c.reset || len(c.explicit) > 0 || len(c.unsetKeys) > 0
	if hasMe {
		var err error
		c.agentDir, err = openExistingWakeAgentDir(root, c.me)
		if err != nil {
			// A missing mailbox or bad handle keeps its own exit code; the wake
			// diagnostic wrap would turn it into a general error.
			var exitErr *ExitCodeError
			if errors.As(err, &exitErr) {
				return err
			}
			return withWakeDiagnostic(err, canonicalWakeRoot(root), c.me)
		}
		defer func() { _ = c.agentDir.Close() }()
	}
	c.canonicalRoot = ""
	if hasMe {
		c.canonicalRoot = canonicalWakeRoot(root)
	}
	if err := c.storeFiles(); err != nil {
		return err
	}
	return c.report()
}

// storeFiles writes the machine or agent settings file a change names and
// reads the files it does not change. agentErr keeps a read error for display.
func (c *wakeConfigRun) storeFiles() error {
	// The machine file: changed here in machine mode, otherwise only read.
	if *c.machine && *c.reset {
		var err error
		if c.machineRaw, err = resetMachineWakeSettings(*c.values, c.explicit); err != nil {
			return err
		}
		c.machineExists = true
	} else if *c.machine && c.changing {
		var err error
		c.machineRaw, err = updateMachineWakeSettings(func(doc *wakeSettingsDoc) error {
			for _, key := range c.unsetKeys {
				if err := doc.unset(key); err != nil {
					return UsageError("--unset: %v", err)
				}
			}
			doc.set(*c.values, c.explicit)
			return nil
		})
		if err != nil {
			return err
		}
		c.machineExists = true
	} else {
		c.machineRaw, c.machineExists, c.machineErr = readMachineWakeSettingsSettled()
	}
	c.machineDigest = wakeSettingsDigest(c.machineRaw, c.machineExists)

	switch {
	case *c.machine:
		if c.agentDir != nil {
			c.agentRaw, c.agentExists, c.agentErr = readWakeSettingsFile(c.agentDir)
		}
	case *c.reset:
		c.agentRaw, c.agentErr = resetWakeSettingsFileInDir(c.agentDir, *c.values, c.explicit)
		if c.agentErr != nil {
			return c.agentErr
		}
		c.agentExists = true
	case c.changing:
		if err := requireValidWakeSettingsFile(c.agentDir); err != nil {
			return err
		}
		if err := refuseSetOnUnreportedWake(c.agentDir, c.canonicalRoot, c.me); err != nil {
			return err
		}
		c.agentRaw, _, c.agentErr = updateWakeSettingsFileInDir(c.agentDir, func(doc *wakeSettingsDoc) error {
			for _, key := range c.unsetKeys {
				if err := doc.unset(key); err != nil {
					return UsageError("--unset: %v", err)
				}
			}
			doc.set(*c.values, c.explicit)
			return nil
		})
		if c.agentErr != nil {
			// The lifecycle guard joins its unlock result onto fn's error,
			// which hides the usage exit code of an invalid merged value.
			var exitErr *ExitCodeError
			if errors.As(c.agentErr, &exitErr) {
				return exitErr
			}
			return c.agentErr
		}
		c.agentExists = true
	default:
		c.agentRaw, c.agentExists, c.agentErr = readWakeSettingsFile(c.agentDir)
	}
	return nil
}

// report shows the layered settings and the running wake's state, waiting
// for the wake to apply the files when asked.
func (c *wakeConfigRun) report() error {
	// A file that cannot be read or decoded is shown, not hidden: it adds no
	// values and its status carries the error.
	machineFile := wakeConfigMachineFileJSON{Status: "ok"}
	machineFile.Path, _ = machineWakeSettingsPath()
	var machineDoc wakeSettingsDoc
	switch {
	case c.machineErr != nil:
		machineFile.Status, machineFile.Error = "refused", c.machineErr.Error()
	case !c.machineExists:
		machineFile.Status = "absent"
	default:
		var derr error
		if machineDoc, derr = decodeWakeSettingsDoc(c.machineRaw); derr == nil {
			_, _, derr = layerWakeSettings(machineDoc, wakeSettingsDoc{})
		}
		if derr != nil {
			machineDoc = wakeSettingsDoc{}
			machineFile.Status, machineFile.Error = "refused", derr.Error()
		}
	}

	if *c.machine && c.agentDir != nil && machineFile.Status == "ok" && c.machineExists {
		// A wake layers the machine file over this agent's file, so the
		// status must match plain --me; the shown keys stay machine-only.
		var checkDoc wakeSettingsDoc
		if c.agentExists && c.agentErr == nil {
			if d, derr := decodeWakeSettingsDoc(c.agentRaw); derr == nil {
				checkDoc = d
			}
		}
		var scratch wakeConfigFileJSON
		layerWakeConfig(machineDoc, checkDoc, &machineFile, &scratch)
		if machineFile.Status == "refused" {
			machineDoc = wakeSettingsDoc{}
		}
	}

	var file *wakeConfigFileJSON
	var agentDoc wakeSettingsDoc
	if c.agentDir != nil && !*c.machine {
		file = &wakeConfigFileJSON{Status: "ok"}
		if c.agentErr == nil && !c.agentExists {
			file.Status = "absent"
		}
		if c.agentErr == nil && c.agentExists {
			agentDoc, c.agentErr = decodeWakeSettingsDoc(c.agentRaw)
		}
		if c.agentErr != nil {
			agentDoc = wakeSettingsDoc{}
			*file = wakeConfigFileJSON{Status: "refused", Error: c.agentErr.Error()}
		}
	} else if c.agentErr != nil {
		return c.agentErr
	}
	effective, sources := layerWakeConfig(machineDoc, agentDoc, &machineFile, file)

	out := wakeConfigJSON{
		Schema:      1,
		Agent:       c.me,
		Root:        c.canonicalRoot,
		Settings:    make(map[string]wakeConfigSettingJSON, len(wakeSettingDefs)),
		RestartOnly: wakeRestartOnlyFlags,
		File:        file,
		MachineFile: machineFile,
	}
	for _, def := range wakeSettingDefs {
		out.Settings[def.key] = wakeConfigSettingJSON{
			Value:  formatWakeSettingValue(def, effective),
			Source: sources[def.key],
		}
	}
	var waitErr error
	if c.agentDir != nil {
		agentDigest := wakeSettingsDigest(c.agentRaw, c.agentExists)
		state, err := inspectWakeSettingsRunState(c.agentDir, c.canonicalRoot, c.me, agentDigest, c.machineDigest)
		if err != nil {
			return err
		}
		if *c.wait && state.Status != wakeSettingsRunNone {
			state, waitErr = waitWakeSettingsApplied(c.agentDir, c.canonicalRoot, c.me, agentDigest, c.machineDigest, *c.machine, *c.timeout)
		}
		out.Wake = &wakeConfigWakeJSON{
			Status:       state.Status,
			Generation:   state.Generation,
			Error:        state.Error,
			MachineState: state.MachineStatus,
			MachineError: state.MachineError,
		}
	}
	if err := printWakeConfig(c.common.JSON, out); err != nil {
		return err
	}
	return waitErr
}

// wakeConfigRun is one wake config invocation: its flags, the handle and
// directory it acts on, and the settings files it read or wrote.
type wakeConfigRun struct {
	common        *commonFlags
	values        *wakeSettings
	machine       *bool
	unsetKeys     multiStringFlag
	reset         *bool
	wait          *bool
	timeout       *time.Duration
	me            string
	explicit      []string
	changing      bool
	agentDir      *wakeAgentDir
	canonicalRoot string
	machineRaw    []byte
	machineExists bool
	machineErr    error
	machineDigest string
	agentRaw      []byte
	agentExists   bool
	agentErr      error
}

// layerWakeConfig merges the layers for display. A layer that cannot merge is
// shown refused and left out, as a wake would: an invalid merge refuses the
// machine layer first (the agent file over the defaults still applies), and
// only then the agent file.
func layerWakeConfig(
	machine, agent wakeSettingsDoc,
	machineFile *wakeConfigMachineFileJSON,
	file *wakeConfigFileJSON,
) (wakeSettings, map[string]string) {
	effective, sources, err := layerWakeSettings(machine, agent)
	if err == nil {
		return effective, sources
	}
	if machineFile.Status == "ok" {
		if effective, sources, rerr := layerWakeSettings(wakeSettingsDoc{}, agent); rerr == nil {
			machineFile.Status, machineFile.Error = "refused", err.Error()
			machineFile.merge = true
			return effective, sources
		}
	}
	if file != nil {
		*file = wakeConfigFileJSON{Status: "refused", Error: err.Error()}
	}
	if effective, sources, rerr := layerWakeSettings(machine, wakeSettingsDoc{}); rerr == nil {
		return effective, sources
	}
	effective, sources, _ = layerWakeSettings(wakeSettingsDoc{}, wakeSettingsDoc{})
	return effective, sources
}

// pollWakeSettings reads the run state of the wake every poll interval until
// decide reports done, or the timeout (0 means none) passes. The generation is
// read from the lock on every poll, so a wake replaced during the poll is
// followed. graceOver is true once the state has stayed unreported for
// wakeConfigUnreportedGrace: a starting wake records its first status just
// after it takes the lock, and a failed status write retries on the next
// tick, so only a wake that stays silent past that window is unreported for
// good.
func pollWakeSettings(
	agentDir *wakeAgentDir,
	root, me, digest, machineDigest string,
	timeout time.Duration,
	decide func(state wakeSettingsRunState, graceOver bool) (bool, error),
) (state wakeSettingsRunState, timedOut bool, err error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	var unreportedSince time.Time
	for {
		state, err = inspectWakeSettingsRunState(agentDir, root, me, digest, machineDigest)
		if err != nil {
			return state, false, err
		}
		graceOver := false
		if state.Status == wakeSettingsRunUnreported {
			if unreportedSince.IsZero() {
				unreportedSince = time.Now()
			} else {
				graceOver = time.Since(unreportedSince) >= wakeConfigUnreportedGrace
			}
		} else {
			unreportedSince = time.Time{}
		}
		if done, derr := decide(state, graceOver); done {
			return state, false, derr
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return state, true, nil
		}
		time.Sleep(wakeConfigWaitPollInterval)
	}
}

// waitWakeSettingsApplied polls until the running wake reports both file
// digests applied or refused, or no wake runs. A refusal of the file the
// command did not target is a warning; a refusal of the targeted file is an
// error.
func waitWakeSettingsApplied(
	agentDir *wakeAgentDir,
	root, me, digest, machineDigest string,
	machineTarget bool,
	timeout time.Duration,
) (wakeSettingsRunState, error) {
	state, timedOut, err := pollWakeSettings(agentDir, root, me, digest, machineDigest, timeout,
		func(state wakeSettingsRunState, graceOver bool) (bool, error) {
			switch state.Status {
			case wakeSettingsRunApplied:
				if state.MachineStatus == wakeConfigMachineRefused {
					if !machineTarget {
						// The wake applied the agent change; the refused
						// machine file is not what this command changed.
						_ = writeWakeLogLine("warning: wake refused the machine settings file: %s\n", state.MachineError)
						return true, nil
					}
					return true, fmt.Errorf("wake refused the machine settings file: %s", state.MachineError)
				}
				return true, nil
			case wakeSettingsRunNone:
				return true, nil
			case wakeSettingsRunRefused:
				if machineTarget && state.MachineStatus != wakeConfigMachineRefused {
					// The wake applied the machine change; the refused agent
					// file is not what this command changed.
					_ = writeWakeLogLine("warning: wake refused the agent settings file: %s\n", state.Error)
					return true, nil
				}
				return true, fmt.Errorf("wake refused the settings file: %s", state.Error)
			case wakeSettingsRunUnreported:
				if graceOver {
					return true, ActionRequiredError("%s", wakeConfigUnreportedText)
				}
			}
			return false, nil
		})
	if timedOut {
		return state, TimeoutError("wake config --wait timed out after %s; the running wake has not applied the settings", timeout)
	}
	return state, err
}

const wakeConfigUnreportedText = "running wake does not report live settings (an older image, or its status write failed, or a resumed wake is still storing its command-line settings in the file); restart it to use live settings, or wait a moment if it just resumed"

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

// refuseSetOnUnreportedWake stops set and --unset from writing a partial file
// over a live wake that may run command-line settings the absent file does not
// hold: an unreported wake never seeded the file from its flags.
func refuseSetOnUnreportedWake(agentDir *wakeAgentDir, root, me string) error {
	_, exists, err := readWakeSettingsFile(agentDir)
	if err != nil || exists {
		return err
	}
	// Poll for the startup grace first: a wake just started records its
	// first status, and a resume seeds the file, a moment after the lock.
	// The machine layer is irrelevant here: this gate guards only unseeded
	// command-line settings, so a sidecar without machine fields (a 0.94.0
	// wake) that matches the agent digest is not unreported.
	digest := wakeSettingsDigest(nil, false)
	_, _, err = pollWakeSettings(agentDir, root, me, digest, wakeSettingsDigestAbsent, 0,
		func(state wakeSettingsRunState, graceOver bool) (bool, error) {
			if state.Status != wakeSettingsRunUnreported {
				return true, nil
			}
			if graceOver {
				return true, ActionRequiredError("the running wake does not report live settings and may run command-line settings that are not in the file (an older image, a failed status write, or a resume still storing them in the file); restart it first or wait for a resume to finish (a resume seeds the file from its flags), or use amq wake config --reset with the full set")
			}
			return false, nil
		})
	return err
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
	err = withWakeLifecycleGuardInDir(
		agentDir,
		wakeGuardOptions{noWait: true, retryTimeout: wakeSettingsGuardTimeout},
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

func printWakeConfig(asJSON bool, out wakeConfigJSON) error {
	if asJSON {
		return writeJSON(os.Stdout, out)
	}
	if out.File != nil {
		fileLine := "file: " + out.File.Status
		if out.File.Status == "refused" {
			fileLine += ": " + out.File.Error
		}
		if err := writeStdoutLine(fileLine); err != nil {
			return err
		}
	}
	machineLine := "machine file: " + out.MachineFile.Status
	if out.MachineFile.Status == "refused" {
		machineLine += ": " + out.MachineFile.Error
	}
	if out.MachineFile.Path != "" {
		machineLine += " (" + out.MachineFile.Path + ")"
	}
	if err := writeStdoutLine(machineLine); err != nil {
		return err
	}
	if out.MachineFile.Status == "refused" {
		note := "a running wake keeps its last good machine settings; a wake that starts now runs without them"
		if out.MachineFile.merge {
			note = "this agent's wake runs without the machine settings because they conflict with its agent file"
		}
		if err := writeStdoutLine(note); err != nil {
			return err
		}
	}
	for _, def := range wakeSettingDefs {
		setting := out.Settings[def.key]
		if err := writeStdout("%-20s %v (%s)\n", def.key, setting.Value, setting.Source); err != nil {
			return err
		}
	}
	if out.Wake == nil {
		return nil
	}
	line := "wake: " + out.Wake.Status
	switch out.Wake.Status {
	case wakeSettingsRunNone:
		line = "wake: no running wake"
	case wakeSettingsRunRefused:
		line = "wake: refused: " + out.Wake.Error
	case wakeSettingsRunUnreported:
		line = "wake: " + wakeConfigUnreportedText
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	if out.Wake.MachineState == wakeConfigMachineRefused {
		return writeStdoutLine("wake machine file: refused: " + out.Wake.MachineError)
	}
	return nil
}
