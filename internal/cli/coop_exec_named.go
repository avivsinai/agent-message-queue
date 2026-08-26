package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

type coopNamedMode int

const (
	coopNamedModeOff coopNamedMode = iota
	coopNamedModeArgv
	coopNamedModeTUI
	coopNamedModeUnknown
)

type coopNamedResumeSyntax = launch.ResumeSyntax

const (
	coopNamedResumeFlags  = launch.ResumeSyntaxFlags
	coopNamedResumeCodex  = launch.ResumeSyntaxCodex
	coopNamedResumeCursor = launch.ResumeSyntaxCursor
)

type coopNamedHarness struct {
	mode         coopNamedMode
	resumeSyntax coopNamedResumeSyntax
}

var coopNamedHarnesses = map[string]coopNamedHarness{
	launch.ClaudeProvider: {mode: coopNamedModeArgv, resumeSyntax: coopNamedResumeFlags},
	"pi":                  {mode: coopNamedModeArgv, resumeSyntax: coopNamedResumeFlags},
	launch.CodexProvider:  {mode: coopNamedModeTUI, resumeSyntax: coopNamedResumeCodex},
	"agent":               {mode: coopNamedModeTUI, resumeSyntax: coopNamedResumeCursor},
	launch.CursorProvider: {mode: coopNamedModeTUI, resumeSyntax: coopNamedResumeCursor},
	launch.GrokProvider:   {mode: coopNamedModeUnknown, resumeSyntax: coopNamedResumeFlags},
}

func coopNamedHarnessFor(binary string) (coopNamedHarness, bool) {
	harness, ok := coopNamedHarnesses[strings.ToLower(filepath.Base(binary))]
	return harness, ok
}

func coopNamedModeFor(binary string) coopNamedMode {
	if harness, ok := coopNamedHarnessFor(binary); ok {
		return harness.mode
	}
	return coopNamedModeUnknown
}

func agentArgsHasNameFlag(args []string) bool {
	return launch.ArgsHaveNameFlag(args)
}

func agentArgsPreventAutoName(args []string) bool {
	return agentArgsPreventAutoNameFor("", args)
}

func agentArgsPreventAutoNameFor(cmdName string, args []string) bool {
	resumeSyntax := coopNamedResumeFlags
	if harness, ok := coopNamedHarnessFor(cmdName); ok {
		resumeSyntax = harness.resumeSyntax
	}
	return agentArgsHasNameFlag(args) || agentArgsHaveResume(args, resumeSyntax)
}

func agentArgsHaveResume(args []string, syntax coopNamedResumeSyntax) bool {
	return launch.ArgsHaveResume(args, syntax)
}

func injectCoopNamedArgv(cmdName string, agentArgs []string, me string) []string {
	if coopNamedModeFor(cmdName) != coopNamedModeArgv || agentArgsPreventAutoNameFor(cmdName, agentArgs) {
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
	if agentArgsPreventAutoNameFor(cmdName, agentArgs) {
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
