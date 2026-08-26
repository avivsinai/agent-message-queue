package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type coopNamedMode int

const (
	coopNamedModeOff coopNamedMode = iota
	coopNamedModeArgv
	coopNamedModeTUI
	coopNamedModeUnknown
)

func coopNamedModeFor(binary string) coopNamedMode {
	switch strings.ToLower(filepath.Base(binary)) {
	case "claude", "pi":
		return coopNamedModeArgv
	case "codex", "agent":
		return coopNamedModeTUI
	default:
		return coopNamedModeUnknown
	}
}

func agentArgsHasNameFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if coopNamedValueFlag(arg) {
			i++
			continue
		}
		if arg == "-n" || arg == "--name" || strings.HasPrefix(arg, "--name=") {
			return true
		}
	}
	return false
}

func agentArgsPreventAutoName(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if coopNamedValueFlag(arg) {
			i++
			continue
		}
		if arg == "-n" || arg == "--name" || strings.HasPrefix(arg, "--name=") ||
			arg == "-r" || arg == "--resume" || strings.HasPrefix(arg, "--resume=") ||
			arg == "-c" || arg == "--continue" || strings.HasPrefix(arg, "--continue=") {
			return true
		}
	}
	return false
}

func coopNamedValueFlag(arg string) bool {
	switch arg {
	case "--model", "-m", "--config", "--cwd", "--output-format", "--permission-mode", "--settings", "--system-prompt", "--append-system-prompt", "--session":
		return true
	default:
		return false
	}
}

func injectCoopNamedArgv(cmdName string, agentArgs []string, me string) []string {
	if coopNamedModeFor(cmdName) != coopNamedModeArgv || agentArgsPreventAutoName(agentArgs) {
		return agentArgs
	}
	return append([]string{"--name", me}, agentArgs...)
}

func coopNamedSessionLabel(session, handle string) string {
	if session == "" {
		return handle
	}
	return session + "/" + handle
}

func resolveCoopNamedEnabled(fsFlagVisited bool, flagValue bool) (bool, error) {
	if fsFlagVisited {
		return flagValue, nil
	}
	if raw, ok := os.LookupEnv("AMQ_COOP_NAMED"); ok {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "0", "false", "off", "no":
			return false, nil
		case "1", "true", "on", "yes":
			return true, nil
		default:
			return false, UsageError("AMQ_COOP_NAMED must be 0 or 1")
		}
	}
	config, present, err := loadProjectLaunchConfig()
	if err != nil {
		return false, err
	}
	if present && config.Named != nil {
		return *config.Named, nil
	}
	return true, nil
}

func coopNamedTUICommand(binaryBase, me string) string {
	switch strings.ToLower(filepath.Base(binaryBase)) {
	case "pi":
		return "/name " + me
	default:
		return "/rename " + me
	}
}

func coopNamedUnknownReminder(me, binary string) string {
	return fmt.Sprintf(
		"amq coop exec --named: name this CLI session %q manually (unknown binary %q)",
		me,
		filepath.Base(binary),
	)
}

func applyCoopNamedBeforeExec(
	named bool,
	cmdName string,
	agentArgs []string,
	me string,
) ([]string, error) {
	return applyCoopNamedBeforeExecAt(named, cmdName, agentArgs, me, time.Now())
}

func applyCoopNamedBeforeExecAt(
	named bool,
	cmdName string,
	agentArgs []string,
	name string,
	execStart time.Time,
) ([]string, error) {
	if !named {
		return agentArgs, nil
	}
	switch coopNamedModeFor(cmdName) {
	case coopNamedModeArgv:
		return injectCoopNamedArgv(cmdName, agentArgs, name), nil
	case coopNamedModeTUI:
		if err := startCoopNamedTUIInjector(name, cmdName, execStart); err != nil {
			_ = writeStderr("warning: coop named inject: %v\n", err)
		}
		return agentArgs, nil
	case coopNamedModeUnknown:
		_ = writeStderr("%s\n", coopNamedUnknownReminder(name, cmdName))
		return agentArgs, nil
	default:
		return agentArgs, nil
	}
}
