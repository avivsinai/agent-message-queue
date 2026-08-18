package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type upgradeStaleWake struct {
	Root string
	Hint opsHint
}

const upgradeLiveWakePreviousBinaryNote = "Live wakes started by the previous binary stay bound to that image; retire them first, or run amq doctor --ops --fix-wake-locks after the old install directory is gone."

func reportStaleWakesAfterUpgrade() error {
	if err := writeStdoutLine(upgradeLiveWakePreviousBinaryNote); err != nil {
		return err
	}
	root, err := resolveUpgradeDiagnosticRoot()
	if err != nil || strings.TrimSpace(root) == "" {
		return nil
	}

	stale := collectUpgradeStaleWakes(root)
	if len(stale) == 0 {
		return nil
	}
	if err := writeStdoutLine("Stale running wakes:"); err != nil {
		return err
	}
	for _, wake := range stale {
		if err := writeStdoutLine(fmt.Sprintf("  - %s: %s", wake.Root, wake.Hint.Message)); err != nil {
			return err
		}
	}
	return nil
}

func resolveUpgradeDiagnosticRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv(envRoot)); root != "" {
		return absPath(resolveRoot(root)), nil
	}
	root, found, err := resolveDiscoveredBaseRoot()
	if err != nil || !found {
		return "", err
	}
	return absPath(resolveRoot(root)), nil
}

func collectUpgradeStaleWakes(root string) []upgradeStaleWake {
	var stale []upgradeStaleWake
	for _, candidate := range upgradeDiagnosticRoots(root) {
		agents := discoveredWakeLockAgents(candidate, nil)
		_, hints := checkWakeLocksWithHints(candidate, agents, false)
		for _, hint := range hints {
			if hint.Code != "stale_wake_binary" || hint.WakeBinary == nil {
				continue
			}
			stale = append(stale, upgradeStaleWake{Root: candidate, Hint: hint})
		}
	}
	return stale
}

func upgradeDiagnosticRoots(root string) []string {
	root = absPath(resolveRoot(root))
	base := baseRootOfForDisplay(root)
	if base == "" {
		base = root
	}

	seen := make(map[string]struct{})
	roots := make([]string, 0, 2)
	add := func(candidate string) {
		candidate = absPath(resolveRoot(candidate))
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		roots = append(roots, candidate)
	}

	add(base)
	entries, err := os.ReadDir(base)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || validateSessionName(entry.Name()) != nil {
				continue
			}
			candidate := filepath.Join(base, entry.Name())
			if dirExists(filepath.Join(candidate, "agents")) {
				add(candidate)
			}
		}
	}
	add(root)
	sort.Strings(roots)
	return roots
}
