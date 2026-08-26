package launch

import "strings"

type ResumeSyntax uint8

const (
	ResumeSyntaxFlags ResumeSyntax = iota
	ResumeSyntaxCodex
	ResumeSyntaxCursor
)

func ArgsHaveNameFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if argvValueFlag(arg) {
			i++
			continue
		}
		if arg == "-n" || arg == "--name" || strings.HasPrefix(arg, "--name=") {
			return true
		}
	}
	return false
}

func ArgsHaveResume(args []string, syntax ResumeSyntax) bool {
	switch syntax {
	case ResumeSyntaxCodex:
		return argsHaveCodexResume(args)
	case ResumeSyntaxCursor:
		return argsHaveResumeFlags(args, false)
	default:
		return argsHaveResumeFlags(args, true)
	}
}

func argsHaveResumeFlags(args []string, allowContinue bool) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if argvValueFlag(arg) {
			i++
			continue
		}
		if arg == "-r" || arg == "--resume" || strings.HasPrefix(arg, "--resume=") ||
			allowContinue && (arg == "-c" || arg == "--continue" || strings.HasPrefix(arg, "--continue=")) {
			return true
		}
	}
	return false
}

func argsHaveCodexResume(args []string) bool {
	var firstPositional string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if argvValueFlag(arg) {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if firstPositional == "" {
			firstPositional = arg
			if arg == "resume" {
				return true
			}
			continue
		}
		return firstPositional == "exec" && arg == "resume"
	}
	return false
}

func argvValueFlag(arg string) bool {
	switch arg {
	case "--model", "-m", "--config", "--cwd", "--output-format", "--permission-mode", "--settings", "--system-prompt", "--append-system-prompt", "--session":
		return true
	default:
		return false
	}
}
