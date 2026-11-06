package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
)

// doctor --fix repairs the links doctor can repair without the owner, each
// step idempotent, and returns one line per change: the approval hook, the
// Stop hook (where native mode needs it) and the wake dialog guard in every
// Claude settings file, the Codex dialog guard, the machine agent's import
// file while no machine agent exists, and the login service for `up` while
// none is installed. It never restarts amq-acp or a wake, never deletes a
// request record, and never writes Buzz Desktop's own agent list. An
// approval or Stop hook it installs or keeps records the owner's opt-in, so
// the machine agent restores it when another writer removes it.
func doctorFix(stateDir string, all []binding.Binding, fail reportFunc) []string {
	fixed := []string{}
	changed := func(link, subject, what string) {
		line := link
		if subject != "" {
			line += " " + subject
		}
		fixed = append(fixed, line+": "+what)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fail("hooks", "", "cannot resolve the home directory: "+err.Error(), "set HOME")
		return fixed
	}
	fixHooks(home, stateDir, all, changed, fail)
	fixMachineImport(home, changed, fail)
	fixService(changed, fail)
	return fixed
}

// fixHooks installs or migrates the AMQ hooks in each Claude settings file
// and the Codex dialog guard.
func fixHooks(home, stateDir string, all []binding.Binding, changed func(link, subject, what string), fail reportFunc) {
	bin, _, err := stableExecutable()
	if err != nil {
		fail("hooks", "", "cannot install the hooks from this amq-remote: "+err.Error(), "run the installed `amq-remote doctor --fix`")
		return
	}
	amq, amqErr := stableAMQ()
	native := nativeConfigured(stateDir, all)
	for _, path := range fixSettingsFiles(home, stateDir, all) {
		if wrote, err := claude.EnsurePermissionHook(home, path, bin, claude.DefaultPermissionWait); err != nil {
			fail("hooks", path, "cannot install the approval hook: "+err.Error(), "repair "+path)
		} else if wrote {
			changed("hooks", path, "installed the AMQ approval hook (PermissionRequest)")
		}
		if state, err := claude.StopHookStateAt(path); err == nil && (native || state != claude.StopHookMissing) {
			if wrote, err := claude.EnsureStopHook(home, path, bin); err != nil {
				fail("hooks", path, "cannot install the Stop hook: "+err.Error(), "repair "+path)
			} else if wrote {
				changed("hooks", path, "installed the AMQ Stop hook")
			}
		}
		switch state, _ := amqcli.ClaudeDialogGuardAt(path); state {
		case amqcli.DialogGuardOK, amqcli.DialogGuardDisabled, amqcli.DialogGuardUnsupported:
		default:
			if amqErr != nil {
				fail("hooks", path, "cannot install the wake dialog guard: no amq binary: "+amqErr.Error(), "install amq, then run `amq-remote doctor --fix` again")
				continue
			}
			if wrote, err := amqcli.InstallClaudeDialogGuardAt(path, amq); err != nil {
				fail("hooks", path, "cannot install the wake dialog guard: "+err.Error(), "repair "+path)
			} else if wrote {
				changed("hooks", path, "installed the wake dialog guard")
			}
		}
	}
	if amqErr != nil {
		return
	}
	if path, wrote, trust, err := amqcli.InstallCodexDialogGuard(amq); err != nil {
		fail("hooks", path, "cannot install the Codex wake dialog guard: "+err.Error(), "repair "+path)
	} else if wrote {
		changed("hooks", path, "installed the Codex wake dialog guard; "+trust)
	}
}

// fixSettingsFiles is claudeSettingsFiles plus the settings file the
// environment names, which --fix creates when it is missing.
func fixSettingsFiles(home, stateDir string, all []binding.Binding) []string {
	env := filepath.Join(filepath.Clean(harness.ClaudeConfigDir(home)), "settings.json")
	files := []string{env}
	for _, path := range claudeSettingsFiles(stateDir, all) {
		if canonicalDir(filepath.Dir(path)) != canonicalDir(filepath.Dir(env)) {
			files = append(files, path)
		}
	}
	return files
}

// machineImportName is the import file --fix writes under ~/Downloads.
const machineImportName = "AMQ Remote machine.agent.json"

// fixMachineImport writes the machine agent's Buzz Desktop import file
// while this machine has no machine agent and the file is not there yet.
func fixMachineImport(home string, changed func(link, subject, what string), fail reportFunc) {
	if machineOf(time.Now()).State != agentMissing {
		return
	}
	out := filepath.Join(home, "Downloads", machineImportName)
	if fileExists(out) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		fail("machine_agent", "", "cannot write the import file: "+err.Error(), "run `amq-acp setup --machine --out "+shQuote(out)+"`")
		return
	}
	if err := runMachineSetup(out); err != nil {
		fail("machine_agent", "", "cannot write the import file: "+err.Error(), "run `amq-acp setup --machine --out "+shQuote(out)+"`")
		return
	}
	changed("machine_agent", "", "wrote "+out+"; in Buzz Desktop: Agents, +, Import that file, Start, and turn on Auto-start")
}

// machineSetupTimeout bounds `amq-acp setup --machine`.
const machineSetupTimeout = 15 * time.Second

// runMachineSetup runs the installed `amq-acp setup --machine --out out`,
// which writes the import file and the AMQ Remote harness it names. Tests
// replace it.
var runMachineSetup = func(out string) error {
	acp, err := siblingBinary("amq-acp")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineSetupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, acp, "setup", "--machine", "--out", out)
	cmd.WaitDelay = childWaitDelay
	if raw, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(raw)))
	}
	return nil
}

// fixService installs the login service that runs `up` for the machine
// root while no login service is installed.
func fixService(changed func(link, subject, what string), fail reportFunc) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return
	}
	path, err := loginservice.Path()
	if err != nil {
		return
	}
	if st, err := loginservice.Read(path); err != nil || st.Owned {
		return
	}
	root, err := serviceRoot("", true)
	if err == nil {
		_, err = serviceInstall(root, path, false, io.Discard)
	}
	if err != nil {
		fail("service", path, "cannot install the login service: "+err.Error(), "run `amq-remote service install`")
		return
	}
	changed("service", path, "installed the login service that runs `amq-remote up --root "+root+"` at each login")
}

// siblingBinary is the program name beside this amq-remote, symlinks
// resolved, else the one on PATH.
func siblingBinary(name string) (string, error) {
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			if path := filepath.Join(filepath.Dir(real), name); fileExists(path) {
				return path, nil
			}
		}
	}
	return exec.LookPath(name)
}

// stableAMQ is the amq a hook should run: the one on PATH when it is the
// same file as the amq beside this amq-remote, so an upgrade that removes
// the versioned path keeps the hook working, else that amq.
func stableAMQ() (string, error) {
	amq, err := installedAMQ()
	if err != nil {
		return "", err
	}
	looked, err := exec.LookPath("amq")
	if err == nil {
		looked, err = filepath.Abs(looked)
	}
	if err != nil {
		return amq, nil
	}
	a, errA := os.Stat(amq)
	b, errB := os.Stat(looked)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return looked, nil
	}
	if !filepath.IsAbs(amq) {
		return "", errors.New("the amq binary has no absolute path")
	}
	return amq, nil
}
