package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

func stripNoUpdateCheckArgs(args []string) ([]string, bool) {
	if len(args) == 0 {
		return args, false
	}
	filtered := make([]string, 0, len(args))
	noCheck := false
	for i, arg := range args {
		// Arguments after "--" belong to the command, never to amq.
		if arg == "--" {
			return append(filtered, args[i:]...), noCheck
		}
		if arg == "--no-update-check" {
			noCheck = true
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--no-update-check="); ok {
			// Only an explicit false value keeps the check enabled.
			if disabled, err := strconv.ParseBool(value); err != nil || disabled {
				noCheck = true
			}
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered, noCheck
}

func startUpdateNotifier(command, version string, noCheck bool) {
	if noCheck || update.IsNoUpdateCheckEnv() {
		return
	}
	if command == "upgrade" {
		return
	}
	notifier := update.Notifier{
		CurrentVersion: version,
	}
	notifier.Start(context.Background())
}
