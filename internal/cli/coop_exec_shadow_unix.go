//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
)

const launchVersionProbeBudget = time.Second

var stableHarnessVersion = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// warnShadowedHarness is advisory: it never changes the selected executable,
// PATH, arguments, or launch disposition. One deadline bounds all probes.
func warnShadowedHarness(command, selected string) {
	facts, known := harness.ForExecutable(command)
	if !known {
		return
	}
	first, err := os.Stat(selected)
	if err != nil {
		return
	}
	alternatives := laterExecutablePaths(filepath.Base(command), first)
	if len(alternatives) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), launchVersionProbeBudget)
	defer cancel()
	current, currentOrder, ok := launchHarnessVersion(ctx, selected, facts.Name)
	if !ok {
		return
	}
	for _, candidate := range alternatives {
		if ctx.Err() != nil {
			return
		}
		version, order, ok := launchHarnessVersion(ctx, candidate, facts.Name)
		if ok && codexVersionNewer(order, currentOrder) {
			_ = writeStderr("warning: PATH selects %s %q (%s); newer %q (%s) is later in PATH; put the intended installation first in PATH\n", facts.Name, selected, current, candidate, version)
			return
		}
	}
}

func launchHarnessVersion(ctx context.Context, executable, name string) (string, [3]uint64, bool) {
	output, ok := probeHarnessVersion(ctx, executable)
	if !ok {
		return "", [3]uint64{}, false
	}
	if name == harness.Cursor {
		date, hash, found := strings.Cut(output, "-")
		parsed, err := time.Parse("2006.01.02", date)
		if !found || hash == "" || strings.Trim(hash, "0123456789abcdefABCDEF") != "" || err != nil {
			return "", [3]uint64{}, false
		}
		return output, [3]uint64{uint64(parsed.Year()), uint64(parsed.Month()), uint64(parsed.Day())}, true
	}
	for _, field := range strings.Fields(output) {
		parts := stableHarnessVersion.FindStringSubmatch(field)
		if parts == nil {
			continue
		}
		var order [3]uint64
		for i := range order {
			value, err := strconv.ParseUint(parts[i+1], 10, 64)
			if err != nil {
				return "", order, false
			}
			order[i] = value
		}
		return fmt.Sprintf("%d.%d.%d", order[0], order[1], order[2]), order, true
	}
	return "", [3]uint64{}, false
}
