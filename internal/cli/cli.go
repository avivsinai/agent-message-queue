package cli

// cli.go defines the top-level command dispatcher and usage text.

const (
	envRoot     = "AM_ROOT"
	envMe       = "AM_ME"
	envBaseRoot = "AM_BASE_ROOT"
)

func Run(args []string, version string) error {
	args, noUpdate := stripNoUpdateCheckArgs(args)
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return printUsageRegistry()
	}
	if args[0] == "help" {
		return routeHelp(args[1:])
	}
	if isVersionArg(args[0]) {
		return printVersion(version)
	}
	startUpdateNotifier(args[0], version, noUpdate)

	// upgrade needs version, handle separately.
	if args[0] == "upgrade" {
		return runUpgrade(args[1:], version)
	}

	cmd := findCommand(args[0])
	if cmd == nil {
		return UsageError("unknown command: %s. Run 'amq --help' for available commands", args[0])
	}
	return cmd.Handler(args[1:])
}

// routeHelp dispatches "amq help [path...]" to the appropriate command's --help.
func routeHelp(path []string) error {
	if len(path) == 0 {
		return printUsageRegistry()
	}

	// Special-case upgrade (needs version, not in standard dispatch).
	if path[0] == "upgrade" {
		if len(path) > 1 {
			return UsageError("unknown upgrade subcommand: %s. Run 'amq upgrade --help' for details", path[1])
		}
		return runUpgrade([]string{"--help"}, "")
	}

	cmd := findCommand(path[0])
	if cmd == nil {
		return UsageError("unknown command: %s. Run 'amq --help' for available commands", path[0])
	}

	if len(path) == 1 {
		return cmd.Handler([]string{"--help"})
	}

	// Only subcommand groups accept a second path segment.
	if len(cmd.Children) == 0 {
		return UsageError("command %q has no subcommands. Run 'amq %s --help' for details", path[0], path[0])
	}

	if len(path) > 2 {
		return UsageError("too many arguments. Run 'amq %s %s --help' for details", path[0], path[1])
	}

	// Pass "subcommand --help" to the group handler.
	return cmd.Handler([]string{path[1], "--help"})
}

func isVersionArg(arg string) bool {
	switch arg {
	case "--version", "-v", "version":
		return true
	default:
		return false
	}
}

func printVersion(version string) error {
	return writeStdoutLine(version)
}

// formatUnknownSubcommand returns a consistent error for unknown subcommands.
func formatUnknownSubcommand(group, sub string) error {
	return UsageError("unknown %s subcommand: %s. Run 'amq %s --help' for available subcommands", group, sub, group)
}
