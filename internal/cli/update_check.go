package cli

import (
	"context"
	"strconv"
	"strings"
	"time"

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
	waitUpdateCheck = notifier.Start(context.Background())
}

// updateCheckBudget bounds how long a finished command waits for a started
// update refresh to land in the cache.
const updateCheckBudget = 750 * time.Millisecond

// waitUpdateCheck waits for the refresh startUpdateNotifier started; it is set
// once, before the command runs.
var waitUpdateCheck = func(time.Duration) {}

// WaitUpdateCheck lets a started update refresh finish within a small budget.
// The process calls it once after Run returns, before it exits.
func WaitUpdateCheck() { waitUpdateCheck(updateCheckBudget) }
