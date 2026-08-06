//go:build darwin || linux

package cli

import "strings"

func authorizeTerminalWritePlatform(cfg *wakeConfig) bool {
	allowed, _ := authorizeTerminalWritePlatformState(cfg)
	return allowed
}

func classifyWakeGenerationPlatformState(
	cfg *wakeConfig,
) (wakeLockInspection, bool, error) {
	current := inspectWakeLock(cfg.root, cfg.me)
	if !current.Exists {
		if strings.TrimSpace(cfg.terminalGeneration) != "" {
			return current, false, newWakeTerminalAuthorityLoss(
				"wake generation disappeared",
			)
		}
		return current, false, nil
	}
	if strings.TrimSpace(current.Lock.Generation) == "" {
		// An empty or unreadable generation is not owner-identity evidence.
		// Park input without silencing attention while ownership is inconclusive.
		return current, false, nil
	}
	if strings.TrimSpace(cfg.terminalGeneration) == "" {
		cfg.terminalGeneration = current.Lock.Generation
		cfg.terminalTTY = current.Lock.TTY
	}
	if current.Lock.Generation != cfg.terminalGeneration {
		// A readable different generation is positive replacement evidence even
		// when a newer lock schema is otherwise unverified. Mixed-version
		// upgrades must supersede the old narrator instead of preserving it.
		return current, false, newWakeTerminalAuthorityLoss(
			"wake generation changed",
		)
	}
	return current, true, nil
}

func authorizeTerminalWritePlatformState(cfg *wakeConfig) (bool, error) {
	current, generationCurrent, err := classifyWakeGenerationPlatformState(cfg)
	if err != nil || !generationCurrent {
		return false, err
	}
	if strings.TrimSpace(current.Lock.TTY) == "" ||
		strings.TrimSpace(cfg.terminalTTY) == "" {
		// Absent TTY metadata cannot authorize input even when both missing
		// values compare equal. The literal legacy value "unknown" remains a
		// deliberate non-empty fail-open capability.
		return false, nil
	}
	if current.Lock.TTY != cfg.terminalTTY {
		// The generation still belongs to this wake, so changed or absent TTY
		// metadata cannot prove that a replacement narrator exists. Park input
		// while keeping bounded attention alive.
		return false, nil
	}
	switch wakeLockTerminalAttachment(current) {
	case wakeTerminalGone:
		// The retained terminal capability may outlive controlling-terminal
		// attachment. Preserve the legacy unknown-name fail-open policy; a
		// concrete path still fails because it can no longer match current.
		return !strings.HasPrefix(cfg.terminalTTY, "/dev/"), nil
	case wakeTerminalAttached, wakeTerminalUndeterminable:
		// A concrete path must still name this process's current terminal.
		// Legacy "unknown" locks retain the prior fail-open behavior because
		// writes use the wake's already-bound controlling-terminal capability.
		return !strings.HasPrefix(cfg.terminalTTY, "/dev/") ||
			getWakeCurrentTTY() == cfg.terminalTTY, nil
	default:
		return false, nil
	}
}
