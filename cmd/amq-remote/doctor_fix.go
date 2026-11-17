package main

import (
	"errors"
	"io"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/dialogguard"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
)

// doctor --fix repairs what the check pass flags and doctor can repair
// without the owner, each step idempotent, and returns one line per change:
// the approval hook, the Stop hook (where native mode needs it) and the
// wake dialog guard in each Claude settings file doctor checks, and, for
// the machine root only, the login service for `up` while doctor advises one
// and none is installed. It never creates a settings file in a Claude
// config dir that does not exist, never installs a hook the owner removed
// with its uninstall command (doctor notes it), never restarts amq-acp or a
// wake, and never deletes a request record. An approval or Stop hook it
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
		switch state, _ := dialogguard.ClaudeAt(path); state {
		case dialogguard.OK, dialogguard.Disabled, dialogguard.Unsupported:
		default:
			if amqErr != nil {
				fail("hooks", path, "cannot install the wake dialog guard: no amq binary: "+amqErr.Error(), "install amq, then run `amq-remote doctor --fix` again")
				continue
			}
			if wrote, err := dialogguard.InstallClaudeAt(path, amq); err != nil {
				fail("hooks", path, "cannot install the wake dialog guard: "+err.Error(), "repair "+path)
			} else if wrote {
				changed("hooks", path, "installed the wake dialog guard")
			}
		}
	}
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
