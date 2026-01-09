package cli

import (
	"context"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

func stripNoUpdateCheckArgs(args []string) ([]string, bool) {
	if len(args) == 0 {
		return args, false
	}
	filtered := make([]string, 0, len(args))
	noCheck := false
	for _, arg := range args {
		if arg == "--no-update-check" || strings.HasPrefix(arg, "--no-update-check=") {
			noCheck = true
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
