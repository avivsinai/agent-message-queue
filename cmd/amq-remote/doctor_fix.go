package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	amqcli "github.com/avivsinai/agent-message-queue/internal/cli"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
)

// doctor --fix repairs what the check pass flags and doctor can repair
// without the owner, each step idempotent, and returns one line per change:
// the approval hook, the Stop hook (where native mode needs it) and the
// wake dialog guard in each Claude settings file doctor checks, the machine agent's import file while no machine agent
// exists, and, for the machine root only, the login service for `up` while
// doctor advises one and none is installed. It never creates a settings file
// in a Claude config dir that does not exist, never installs a hook the
// owner removed with its uninstall command (doctor notes it), never
// restarts amq-acp or a wake, never deletes a request record, and never
// writes Buzz Desktop's own agent list. An approval or Stop hook it
// installs records the owner's opt-in, so the machine agent restores it
// when another writer removes it.
func doctorFix(root, stateDir string, all []binding.Binding, fail reportFunc) []string {
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
	fixService(root, stateDir, changed, fail)
	return fixed
}

// fixHooks installs or migrates the AMQ hooks doctor flags in each Claude
// settings file it checks.
func fixHooks(home, stateDir string, all []binding.Binding, changed func(link, subject, what string), fail reportFunc) {
	bin, _, err := stableExecutable()
	if err != nil {
		fail("hooks", "", "cannot install the hooks from this amq-remote: "+err.Error(), "run the installed `amq-remote doctor --fix`")
		return
	}
	amq, amqErr := claude.HookBinary("amq")
	native := nativeConfigured(stateDir, all)
	ensure := func(path, what string, install func() (bool, error)) {
		switch wrote, err := install(); {
		case errors.Is(err, claude.ErrHookRemoved):
		case err != nil:
			fail("hooks", path, "cannot install the "+what+": "+err.Error(), "repair "+path)
		case wrote:
			changed("hooks", path, "installed the AMQ "+what)
		}
	}
	for _, path := range claudeSettingsFiles(stateDir, all) {
		if state, err := claude.PermissionHookState(path); err == nil {
			if form, _ := claude.PermissionHookForm(home, path); state == claude.HookMissing || claude.FormFlagged(form) {
				ensure(path, "approval hook (PermissionRequest)", func() (bool, error) { return claude.EnsurePermissionHook(home, path, bin) })
			}
		}
		if state, err := claude.StopHookStateAt(path); err == nil {
			if form, _ := claude.StopHookForm(home, path); native && state == claude.HookMissing || state != claude.HookMissing && claude.FormFlagged(form) {
				ensure(path, "Stop hook", func() (bool, error) { return claude.EnsureStopHook(home, path, bin) })
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
}

// machineImportFile is the import file --fix writes, relative to the home
// directory.
const machineImportFile = "Downloads/AMQ Remote machine.agent.json"

// fixMachineImport writes the machine agent's Buzz Desktop import file
// while this machine has no machine agent and the file is not there yet.
func fixMachineImport(home string, changed func(link, subject, what string), fail reportFunc) {
	if machineOf(time.Now()).State != agentMissing {
		return
	}
	out := filepath.Join(home, filepath.FromSlash(machineImportFile))
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
	acp, err := claude.SiblingBinary("amq-acp")
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

// fixService installs the login service that runs `up` for root when root
// is the machine root, doctor advises a login service for it
// (noteNoService), and no login service is installed.
func fixService(root, stateDir string, changed func(link, subject, what string), fail reportFunc) {
	flagged := false
	noteNoService(func(_, _, _, _ string) { flagged = true }, root, stateDir)
	if !flagged {
		return
	}
	machine, err := serviceRoot("", false)
	if err != nil || !sameRoot(machine, root) {
		return
	}
	path, err := loginservice.Path()
	if err != nil {
		return
	}
	if st, err := loginservice.Read(path); err != nil || st.Owned {
		return
	}
	if _, err := serviceInstall(machine, path, false, io.Discard); err != nil {
		fail("service", path, "cannot install the login service: "+err.Error(), "run `amq-remote service install`")
		return
	}
	changed("service", path, "installed the login service that runs `amq-remote up --root "+machine+"` at each login")
}
