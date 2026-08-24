package cli

import (
	"fmt"
	"path/filepath"
	"strings"
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
		if arg == "-n" || arg == "--name" {
			return true
		}
		if strings.HasPrefix(arg, "--name=") {
			return true
		}
	}
	return false
}

func injectCoopNamedArgv(cmdName string, agentArgs []string, me string) []string {
	if coopNamedModeFor(cmdName) != coopNamedModeArgv || agentArgsHasNameFlag(agentArgs) {
		return agentArgs
	}
	return append([]string{"--name", me}, agentArgs...)
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
	if !named {
		return agentArgs, nil
	}
	switch coopNamedModeFor(cmdName) {
	case coopNamedModeArgv:
		return injectCoopNamedArgv(cmdName, agentArgs, me), nil
	case coopNamedModeTUI:
		if err := startCoopNamedTUIInjector(me, cmdName); err != nil {
			_ = writeStderr("warning: coop named inject: %v\n", err)
		}
		return agentArgs, nil
	case coopNamedModeUnknown:
		_ = writeStderr("%s\n", coopNamedUnknownReminder(me, cmdName))
		return agentArgs, nil
	default:
		return agentArgs, nil
	}
}
