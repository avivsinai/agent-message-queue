package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/launchd"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
)

// launchctlNotFound is the exit code of `launchctl print` for a service that
// is not loaded ("Could not find service ... in domain").
const launchctlNotFound = 113

// migrateHomeKeepaliveProcesses are the executables that can hold the
// keepalive home: a supervisor, an attach, an inject, or amq-remote up. Any
// running one blocks the move, whatever its version, because it may have
// resolved the legacy home already and create it again.
var migrateHomeKeepaliveProcesses = []string{"amq-keepalive", "amq-remote"}

type migrateHomeHookFix struct {
	agent  string
	config string
	hook   hookinstall.HookCommand
}

// planMigrateHomeKeepalive plans the move of ~/.amq-keepalive to
// ~/.amq/keepalive and the fix-ups after it. The state comes from disk on
// every run, so a rerun finishes a move that an earlier run started.
func planMigrateHomeKeepalive(part *migrateHomePart) (func() error, error) {
	old, cur, err := registry.HomeCandidates()
	if err != nil {
		return nil, err
	}
	part.From, part.To = old, cur
	oldFound, err := migrateHomePlainDir(part, old)
	if err != nil {
		return nil, err
	}
	curFound, err := migrateHomePlainDir(part, cur)
	if err != nil {
		return nil, err
	}
	if len(part.Blockers) > 0 {
		return nil, nil
	}
	if !oldFound && !curFound {
		return nil, nil
	}

	var residue []string
	if oldFound && curFound {
		var why string
		residue, why = migrateHomeKeepaliveResidue(old, false)
		if why != "" {
			part.block("both_exist", old, fmt.Sprintf("both %s and %s exist, and %s %s", old, cur, old, why),
				fmt.Sprintf("Stop the program that created %s again (an older amq-keepalive, or a supervisor started with --registry %s), "+
					"move anything you need out of it, remove it, then run again.", old, filepath.Join(old, "registry.json")))
			return nil, nil
		}
	}
	rename := oldFound && !curFound

	var installed *launchd.Installed
	plistPath, err := launchd.DefaultPlistPath(launchd.DefaultLabel)
	if err != nil {
		return nil, err
	}
	plistInfo, err := os.Lstat(plistPath)
	if err == nil {
		got, err := launchd.ReadInstalled(plistPath, launchd.DefaultLabel)
		if err != nil {
			part.block("unit_unreadable", plistPath, err.Error(), "Fix or remove the plist, then run again.")
			return nil, nil
		}
		installed = &got
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	plistRewrite := installed != nil && migrateHomeUnder(installed.Options.RegistryPath, old)
	if plistRewrite && (plistInfo.Mode()&os.ModeSymlink != 0 || installed.Edited()) {
		// The rewrite writes the plist from its options. It would drop a
		// hand edit (an extra flag, key or PATH) or replace a symlink.
		part.block("unit_edited", plistPath, "the plist is a symlink or differs from what amq-keepalive install-launchd writes",
			"Run amq-keepalive install-launchd again with amq-keepalive "+migrateHomeVersionFloor+" or later (before the move it writes the legacy registry path, which is correct), "+
				"or restore the file (a regular file, not a symlink), then run again. AMQ does not rewrite an edited plist, because the rewrite would drop the change.")
	}

	var hooks, fixes []migrateHomeHookFix
	for _, agent := range []string{hookinstall.AgentClaude, hookinstall.AgentCodex} {
		config, err := migrateHomeHookConfig(agent)
		if err != nil {
			return nil, err
		}
		owned, err := hookinstall.OwnedSessionStartHooks(config)
		if err != nil {
			part.block("hook_config_unreadable", config, err.Error(), "Fix the file, then run again.")
			continue
		}
		for _, hook := range owned {
			fix := migrateHomeHookFix{agent: agent, config: config, hook: hook}
			hooks = append(hooks, fix)
			if migrateHomeUnder(hook.ScriptPath, old) {
				fixes = append(fixes, fix)
			}
		}
	}

	// The job is bootstrapped again only when this run moves the home or
	// rewrites its plist, because the operator stopped it for that. A job
	// that is stopped for any other reason is the operator's to start; at
	// login, RunAtLoad starts it anyway.
	jobLoaded, jobKnown := false, true
	if runtime.GOOS == "darwin" && (installed != nil || rename || len(residue) > 0) {
		jobLoaded, jobKnown = migrateHomeLaunchdLoaded(launchd.ServiceTarget(launchd.DefaultLabel))
	}
	bootstrap := runtime.GOOS == "darwin" && installed != nil && jobKnown && !jobLoaded && (rename || plistRewrite)
	if runtime.GOOS == "darwin" && installed != nil && jobKnown && !jobLoaded && !bootstrap {
		part.OperatorNext = append(part.OperatorNext, "the supervisor job is not loaded; to start it: launchctl bootstrap gui/"+strconv.Itoa(os.Getuid())+" "+plistPath)
	}
	if !rename && len(residue) == 0 && !plistRewrite && len(fixes) == 0 {
		return nil, nil
	}

	moving := rename || len(residue) > 0
	if moving || plistRewrite {
		if !jobKnown {
			part.block("unit_state_unknown", launchd.ServiceTarget(launchd.DefaultLabel), "launchctl print did not say whether the job is loaded",
				"Check it with launchctl print "+launchd.ServiceTarget(launchd.DefaultLabel)+", then run again.")
		} else if jobLoaded {
			part.block("unit_loaded", launchd.ServiceTarget(launchd.DefaultLabel), "the amq-keepalive supervisor job is loaded",
				"Stop it with launchctl bootout "+launchd.ServiceTarget(launchd.DefaultLabel)+" (keep the plist; do not run amq-keepalive uninstall). The migration bootstraps it again.")
		}
	}

	regDir := cur
	if rename {
		regDir = old
	}
	gate := newMigrateHomeGate()
	names := []string{"amq", "amq-keepalive", "amq-remote"}
	gate.addDirs("PATH", migrateHomePATH(), names...)
	if installed != nil {
		gate.addPath("launchd plist", installed.Options.BinaryPath)
		gate.addPath("launchd plist", installed.Self)
		gate.addPath("launchd plist", installed.Options.AMQPath)
		gate.addDirs("launchd plist PATH", migrateHomeSplitPATH(installed.PATH), names...)
	}
	for _, hook := range hooks {
		// install-hook with a new binary adds a second hook and keeps this
		// one, so the remedy is to remove this entry.
		gate.addPathRemedy(hook.agent+" hook", hook.hook.BinaryPath, fmt.Sprintf(
			"Remove the SessionStart hook with this command from %s: %s. Then run amq-keepalive install-hook --agent %s with amq-keepalive %s or later if you want the hook, and run the migration again.",
			hook.config, hook.hook.Command, hook.agent, migrateHomeVersionFloor))
	}
	migrateHomeWakeInjectors(part, gate, filepath.Join(regDir, "registry.json"))
	servicePath, err := loginservice.Path()
	if err != nil {
		return nil, err
	}
	service, serviceErr := loginservice.Read(servicePath)
	if serviceErr == nil && service.Owned {
		gate.addPath("amq-remote service", service.Exe)
	}
	gate.report(part, migrateHomeKeepaliveRemedy)

	if moving {
		if runtime.GOOS == "windows" {
			part.block("unsupported_platform", runtime.GOOS, "lock probes need flock, which this platform does not have",
				"Keep the legacy home; it continues to work.")
		}
		migrateHomeProcessBlockers(part)
		migrateHomeHeldLockBlockers(part, old)
		migrateHomeForeignUnitBlockers(part, old, installed)
		migrateHomeRemoteServiceBlockers(part, serviceErr == nil && service.Owned)
		if serviceErr == nil && service.Owned {
			part.OperatorNext = append(part.OperatorNext, migrateHomeRemoteServiceStart(servicePath))
		}
		if runtime.GOOS != "darwin" {
			part.OperatorNext = append(part.OperatorNext,
				"start your own amq-keepalive supervisor again, with no --registry or with --registry "+filepath.Join(cur, "registry.json"))
		}
	}

	residueStep, renameStep, plistStep, bootstrapStep := -1, -1, -1, -1
	if len(residue) > 0 {
		residueStep = part.step("remove the leftover lock files in %s", old)
	}
	if rename {
		renameStep = part.step("rename %s to %s", old, cur)
	}
	if plistRewrite {
		plistStep = part.step("rewrite --registry in %s", plistPath)
	}
	hookSteps := make([]int, len(fixes))
	for i, fix := range fixes {
		hookSteps[i] = part.step("rewrite the %s hook in %s", fix.agent, fix.config)
	}
	if bootstrap {
		bootstrapStep = part.step("bootstrap %s", launchd.ServiceTarget(launchd.DefaultLabel))
	}

	return func() error {
		stepFailed := func(err error) error {
			part.block("step_failed", part.To, err.Error(), "Fix the cause, then run amq doctor --migrate-home again to finish.")
			return errMigrateHomeRefused
		}
		if residueStep >= 0 {
			if err := migrateHomeRemoveResidue(part, old, residue); err != nil {
				return err
			}
			part.Steps[residueStep].Done = true
		}
		if renameStep >= 0 {
			if err := migrateHomeRenameKeepalive(part, old, cur); err != nil {
				return err
			}
			part.Steps[renameStep].Done = true
			for _, dir := range []string{filepath.Dir(cur), filepath.Dir(old)} {
				if err := fsq.SyncDir(dir); err != nil {
					part.block("sync_failed", dir, fmt.Sprintf("moved %s to %s, but sync of %s failed: %v", old, cur, dir, err),
						"Run amq doctor --migrate-home again to finish.")
					return errMigrateHomeRefused
				}
			}
			if _, err := os.Lstat(old); err == nil {
				part.block("legacy_recreated", old, "a process created "+old+" again during the move; the home is at "+cur,
					"Stop that process, then run again: the rerun removes the leftover lock files when they hold no registry rows.")
				return errMigrateHomeRefused
			}
		}
		if plistStep >= 0 {
			opts := installed.Options
			opts.RegistryPath = filepath.Join(cur, "registry.json")
			opts.Load = false
			if err := launchd.Install(context.Background(), opts); err != nil {
				return stepFailed(err)
			}
			part.Steps[plistStep].Done = true
		}
		for i, fix := range fixes {
			rel, err := filepath.Rel(old, fix.hook.ScriptPath)
			if err != nil {
				return stepFailed(err)
			}
			opts := hookinstall.Options{
				Agent:      fix.agent,
				ScriptPath: filepath.Join(cur, rel),
				BinaryPath: fix.hook.BinaryPath,
				Timeout:    fix.hook.Timeout,
			}
			if fix.agent == hookinstall.AgentClaude {
				opts.ClaudeConfig = fix.config
			} else {
				opts.CodexConfig = fix.config
			}
			if _, err := hookinstall.Install(opts); err != nil {
				return stepFailed(err)
			}
			part.Steps[hookSteps[i]].Done = true
		}
		if bootstrapStep >= 0 {
			for _, step := range launchd.LoadSteps(launchd.DefaultLabel, plistPath) {
				out, code, err := migrateHomeRun("launchctl", step.Args...)
				if (err != nil || code != 0) && !step.MayFail {
					return stepFailed(fmt.Errorf("launchctl %s: exit %d: %v %s", strings.Join(step.Args, " "), code, err, strings.TrimSpace(out)))
				}
			}
			part.Steps[bootstrapStep].Done = true
		}
		part.Status = migrateHomeFinished
		if renameStep >= 0 {
			part.Status = migrateHomeMoved
		}
		return nil
	}, nil
}

// migrateHomeRenameKeepalive renames the home under the old registry's
// registration and write locks. A process that waits on a lock in the old
// path fails its identity recheck after the rename and writes nothing.
func migrateHomeRenameKeepalive(part *migrateHomePart, old, cur string) error {
	store := registry.New(filepath.Join(old, "registry.json"))
	return store.WithExclusive(func() error {
		migrateHomeProcessBlockers(part)
		migrateHomeHeldLockBlockers(part, old)
		if len(part.Blockers) > 0 {
			return errMigrateHomeRefused
		}
		if _, err := amqhome.EnsureDir(); err != nil {
			part.block("unsafe_path", filepath.Dir(cur), err.Error(), "Make it a plain directory, then run again.")
			return errMigrateHomeRefused
		}
		if err := os.Rename(old, cur); err != nil {
			switch {
			case errors.Is(err, syscall.EXDEV):
				part.block("cross_device", old, fmt.Sprintf("%s and %s are on different filesystems", old, cur),
					"AMQ does not copy the keepalive home. Keep the legacy home; it continues to work.")
				return errMigrateHomeRefused
			case errors.Is(err, os.ErrExist), errors.Is(err, syscall.ENOTEMPTY):
				part.block("both_exist", cur, cur+" appeared during the move", "Run amq doctor --migrate-home --dry-run to see what both homes hold.")
				return errMigrateHomeRefused
			}
			return err
		}
		return nil
	})
}

func migrateHomeKeepaliveRemedy(source string) string {
	floor := migrateHomeVersionFloor
	switch {
	case source == "launchd plist":
		return "Upgrade it to " + floor + " or later, then run amq-keepalive install-launchd again and run the migration again."
	case strings.HasPrefix(source, "wake "):
		return "Upgrade it to " + floor + " or later, then replace the wake with amq-keepalive reattach from the agent's session, and run the migration again."
	case source == "amq-remote service":
		return "Upgrade it to " + floor + " or later, then run amq-remote service install again and run the migration again."
	}
	return "Replace it with " + floor + " or later, or remove it from that PATH, then run again."
}

// migrateHomePlainDir reports whether dir is a plain directory, and blocks on
// a symlink or a non-directory.
func migrateHomePlainDir(part *migrateHomePart, dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		part.block("unsafe_path", dir, dir+" is not a plain directory", "AMQ does not follow or replace it. Make it a plain directory, then run again.")
		return false, nil
	}
	return true, nil
}

func migrateHomeUnder(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func migrateHomeHookConfig(agent string) (string, error) {
	if agent == hookinstall.AgentClaude {
		return hookinstall.DefaultClaudeConfig()
	}
	return hookinstall.DefaultCodexConfig()
}

// migrateHomeKeepaliveResidue lists what old holds when it is only what a
// process left by recreating it: lock files that no process holds, and a
// registry with no rows. Otherwise why names the first thing that is more.
// With own, the caller holds the registry and registration locks, which are
// then not probed.
func migrateHomeKeepaliveResidue(old string, own bool) (paths []string, why string) {
	entries, err := os.ReadDir(old)
	if err != nil {
		return nil, "cannot be read: " + err.Error()
	}
	var dirs []string
	for _, entry := range entries {
		path := filepath.Join(old, entry.Name())
		name := entry.Name()
		switch {
		case name == "registry.json" && entry.Type().IsRegular():
			file, err := registry.New(path).LoadSnapshot()
			if err != nil {
				return nil, "holds a registry that cannot be read: " + err.Error()
			}
			if len(file.Entries) > 0 {
				return nil, fmt.Sprintf("holds a registry with %d entries", len(file.Entries))
			}
		case entry.Type().IsRegular() && migrateHomeLockName(name):
			if own && migrateHomeOwnLock(name) {
				break
			}
			if held, err := registry.ProbeLockHeld(path); err != nil || held {
				return nil, "holds a lock that is held or cannot be probed: " + name
			}
		case name == "claude-print" && entry.IsDir():
			sessions, err := os.ReadDir(path)
			if err != nil {
				return nil, "cannot be read: " + err.Error()
			}
			for _, session := range sessions {
				sessionDir := filepath.Join(path, session.Name())
				files, err := os.ReadDir(sessionDir)
				if !session.IsDir() || err != nil || len(files) != 1 || files[0].Name() != "inject.lock" || !files[0].Type().IsRegular() {
					return nil, "holds claude-print state in " + sessionDir
				}
				lock := filepath.Join(sessionDir, "inject.lock")
				if held, err := registry.ProbeLockHeld(lock); err != nil || held {
					return nil, "holds an inject lock that is held or cannot be probed: " + lock
				}
				paths = append(paths, lock)
				dirs = append(dirs, sessionDir)
			}
			dirs = append(dirs, path)
			continue
		default:
			return nil, "holds " + name
		}
		paths = append(paths, path)
	}
	return append(append(paths, dirs...), old), ""
}

func migrateHomeOwnLock(name string) bool {
	return name == "registry.json.lock" || name == "registry.json.registration.lock"
}

func migrateHomeLockName(name string) bool {
	switch name {
	case "registry.json.lock", "registry.json.registration.lock", ".selfupgrade.lock":
		return true
	}
	return strings.HasPrefix(name, "registry.json.up-") && strings.HasSuffix(name, ".lock")
}

// migrateHomeRemoveResidue removes the leftover old home under its registry
// locks. It scans again first and refuses when the leftover changed since the
// plan. os.Remove refuses a directory that gained an entry, so nothing else
// is removed.
func migrateHomeRemoveResidue(part *migrateHomePart, old string, planned []string) error {
	withoutOwnLocks := func(paths []string) []string {
		return slices.DeleteFunc(slices.Clone(paths), func(path string) bool {
			return filepath.Dir(path) == old && migrateHomeOwnLock(filepath.Base(path))
		})
	}
	return registry.New(filepath.Join(old, "registry.json")).WithExclusive(func() error {
		paths, why := migrateHomeKeepaliveResidue(old, true)
		if why == "" && !slices.Equal(withoutOwnLocks(paths), withoutOwnLocks(planned)) {
			why = "changed since the plan"
		}
		if why != "" {
			part.block("both_exist", old, old+" "+why, "Run amq doctor --migrate-home again.")
			return errMigrateHomeRefused
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil {
				part.block("both_exist", old, fmt.Sprintf("remove leftover %s: %v", path, err), "Inspect "+old+", then run again.")
				return errMigrateHomeRefused
			}
		}
		return nil
	})
}

// migrateHomeWakeInjectors adds the inject_via of each registered wake to the
// gate. A live wake runs it on every delivery.
func migrateHomeWakeInjectors(part *migrateHomePart, gate *migrateHomeGate, regPath string) {
	file, err := registry.New(regPath).LoadSnapshot()
	if err != nil {
		part.block("registry_unreadable", regPath, err.Error(), "Fix the registry, then run again.")
		return
	}
	for _, entry := range file.Entries {
		target, found, err := readWakeTarget(entry.Root, entry.Agent)
		if err != nil {
			part.block("wake_target_unreadable", wakeTargetPath(entry.Root, entry.Agent), err.Error(),
				"Check the wake with amq wake check --me "+entry.Agent+" --root "+entry.Root+", then run again.")
			continue
		}
		if found {
			gate.addPath("wake "+entry.Agent+" "+entry.Root, target.InjectVia)
		}
	}
}

// migrateHomeLaunchdLoaded reports whether target is loaded. known is false
// when launchctl gave neither answer.
func migrateHomeLaunchdLoaded(target string) (loaded, known bool) {
	_, code, err := migrateHomeRun("launchctl", "print", target)
	switch {
	case err != nil:
		return false, false
	case code == 0:
		return true, true
	case code == launchctlNotFound:
		return false, true
	}
	return false, false
}

func migrateHomeProcessBlockers(part *migrateHomePart) {
	out, code, err := migrateHomeRun("ps", "-axo", "pid=,comm=")
	if err != nil || code != 0 {
		part.block("process_list_unknown", "ps", fmt.Sprintf("ps -axo pid=,comm= failed (exit %d, %v)", code, err),
			"Make ps work, then run again.")
		return
	}
	for _, line := range strings.Split(out, "\n") {
		pid, comm, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		name := filepath.Base(strings.TrimSpace(comm))
		if _, err := strconv.Atoi(pid); err != nil || !slices.Contains(migrateHomeKeepaliveProcesses, name) {
			continue
		}
		part.block("process_running", name+" pid "+pid, name+" may hold the legacy home and create it again",
			"Stop it (for an amq-remote login service: stop the service), then run again.")
	}
}

func migrateHomeHeldLockBlockers(part *migrateHomePart, old string) {
	locks, _ := filepath.Glob(filepath.Join(old, "registry.json.up-*.lock"))
	injects, _ := filepath.Glob(filepath.Join(old, "claude-print", "*", "inject.lock"))
	for _, lock := range append(locks, injects...) {
		held, err := registry.ProbeLockHeld(lock)
		if err != nil {
			part.block("lock_unknown", lock, err.Error(), "Check what uses the lock, then run again.")
		} else if held {
			part.block("lock_held", lock, "a running process holds this lock", "Stop the amq-remote up or the inject that holds it, then run again.")
		}
	}
}

// migrateHomeForeignUnitBlockers blocks on any launchd or systemd user unit
// other than the AMQ-owned supervisor plist that names the legacy home: it
// would start a process with that path again.
func migrateHomeForeignUnitBlockers(part *migrateHomePart, old string, installed *launchd.Installed) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	for _, dir := range []string{filepath.Join(home, "Library", "LaunchAgents"), filepath.Join(config, "systemd", "user")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if installed != nil && path == installed.Options.PlistPath {
				continue
			}
			info, err := os.Stat(path)
			if err == nil && info.IsDir() {
				continue
			}
			var data []byte
			if err == nil {
				data, err = os.ReadFile(path)
			}
			if err != nil {
				part.block("foreign_unit_unknown", path, err.Error(), "Make the unit readable or remove it, then run again.")
				continue
			}
			if !bytes.Contains(data, []byte(old)) {
				continue
			}
			part.block("foreign_unit", path, "this unit names "+old, "Change it to use "+part.To+" (or no explicit path), stop it, then run again.")
		}
	}
}

func migrateHomeRemoteServiceBlockers(part *migrateHomePart, installed bool) {
	var target, stop string
	var loaded, known bool
	if runtime.GOOS == "darwin" {
		target = launchd.ServiceTarget(loginservice.Label)
		stop = "launchctl bootout " + target
		loaded, known = migrateHomeLaunchdLoaded(target)
	} else {
		if !installed {
			return
		}
		target = loginservice.Unit
		stop = "systemctl --user stop " + loginservice.Unit
		_, code, err := migrateHomeRun("systemctl", "--user", "is-active", "--quiet", loginservice.Unit)
		loaded, known = code == 0 && err == nil, err == nil && (code == 0 || code == 3 || code == 4)
	}
	switch {
	case !known:
		part.block("unit_state_unknown", target, "cannot tell whether the amq-remote login service is running", "Stop it with "+stop+", then run again.")
	case loaded:
		part.block("unit_loaded", target, "the amq-remote login service is loaded", "Stop it with "+stop+"; start it again after the migration.")
	}
}

func migrateHomeRemoteServiceStart(servicePath string) string {
	if runtime.GOOS == "darwin" {
		return "start the amq-remote login service again: launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()) + " " + servicePath
	}
	return "start the amq-remote login service again: systemctl --user start " + loginservice.Unit
}
