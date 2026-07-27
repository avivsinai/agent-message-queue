//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

func prepareCoopWakeLock(root, agent string, yes bool, remedy string) error {
	inspection := inspectWakeLock(root, agent)
	if !inspection.Exists {
		return nil
	}
	// A live process that affirmative process inspection proves is not this wake
	// must never be signaled or have its lock removed by a coop retry.
	if inspection.Process.Running && wakeProcessProvenNotWake(inspection.Process) {
		return fmt.Errorf("wake lock for %s names a running process proven not to be this wake; refusing to signal or remove it; lock: %s; root: %s; reason: %s", agent, inspection.LockPath, inspection.Root, inspection.Reason)
	}
	if inspection.Status == wakeLockStale {
		if err := removeWakeLockIfUnchanged(inspection); err != nil {
			return fmt.Errorf("remove exact stale wake lock: %w", err)
		}
		return nil
	}

	if isLiveRawOrphan(inspection) {
		state := "running, but its terminal is gone"
		proceed, err := confirmCoopBadWake(
			inspection,
			state,
			"gone",
			yes,
			coopWakeRemedy(inspection, state, remedy),
		)
		if err != nil || !proceed {
			return err
		}
		retired, err := retireLiveRawOrphan(inspection)
		if err != nil {
			return fmt.Errorf("retire identity-confirmed live raw orphan: %w", err)
		}
		if !retired {
			return fmt.Errorf("wake changed before live raw orphan retirement; retry")
		}
		return nil
	}

	if inspection.Status != wakeLockUnverified {
		// Valid non-orphans and creating locks retain their existing fail-closed
		// behavior in the wake acquisition path.
		return nil
	}
	state := "cannot confirm whether it is running"
	proceed, err := confirmCoopBadWake(
		inspection,
		state,
		coopWakeTTYDisplay(inspection),
		yes,
		coopWakeRemedy(inspection, state, remedy),
	)
	if err != nil || !proceed {
		return err
	}

	if err := removeWakeLockIfUnchanged(inspection); err != nil {
		return fmt.Errorf("remove exact unverified wake lock: %w", err)
	}
	_ = writeStderr(
		"warning: superseded unidentified wake helper for %s (pid %d on %s) without signaling it; fresh wake is starting, but duplicate notifications may continue until the old helper exits; stop that helper if duplicates persist\n",
		agent,
		inspection.PID,
		coopWakeTTYDisplay(inspection),
	)
	return nil
}

func confirmCoopBadWake(
	inspection wakeLockInspection,
	state string,
	tty string,
	yes bool,
	remedy string,
) (bool, error) {
	_ = writeStderr(
		"wake for %s is blocking startup and cannot notify you\n"+
			"  lock:    %s\n"+
			"  pid:     %d\n"+
			"  tty:     %s\n"+
			"  age:     %s\n"+
			"  state:   %s\n"+
			"  root:    %s\n",
		inspection.Agent,
		inspection.LockPath,
		inspection.PID,
		tty,
		formatCoopWakeLockAge(inspection.Lock.Started, time.Now()),
		state,
		inspection.Root,
	)
	ok, err := consentToClearWake(yes, remedy)
	if err != nil {
		return false, fmt.Errorf("confirm wake cleanup: %w", err)
	}
	if !ok {
		return false, errors.New("wake cleanup declined")
	}
	return true, nil
}

// consentToClearWake reports whether the caller has authorised clearing a
// blocking wake. Headless callers must pass -y; a prompt nobody can answer
// is never shown, and never silently answered.
func consentToClearWake(yes bool, remedy string) (bool, error) {
	if yes {
		return true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		_ = writeStderr("amq coop exec: %s\n", remedy)
		return false, nil
	}
	return confirmPrompt("Clear it and start a fresh wake?")
}

func coopWakeTTYDisplay(inspection wakeLockInspection) string {
	tty := strings.TrimSpace(inspection.Lock.TTY)
	if tty == "" {
		return "unknown"
	}
	return tty
}

func coopWakeRemedy(inspection wakeLockInspection, state, command string) string {
	return fmt.Sprintf("wake for %s is blocking startup and cannot notify you.\n  lock: %s\n  state: %s\n\nRe-run with -y to clear it and start a fresh wake:\n  %s\n\nTo inspect first:\n  AM_ROOT=%s amq doctor --ops", inspection.Agent, inspection.LockPath, state, command, inspection.Root)
}

func coopWakeRemedyForCommand(root, agent, command string, commandArgs []string) string {
	quoted := []string{"amq", "coop", "exec", "-y", "--root", root, "--me", agent, command}
	for _, arg := range commandArgs {
		quoted = append(quoted, arg)
	}
	parts := make([]string, 0, len(quoted))
	for _, arg := range quoted {
		parts = append(parts, shellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func formatCoopWakeLockAge(started string, now time.Time) string {
	startedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(started))
	if err != nil || startedAt.After(now) {
		return "unknown"
	}
	age := now.Sub(startedAt)
	switch {
	case age >= 24*time.Hour:
		days := int(age / (24 * time.Hour))
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	case age >= time.Hour:
		hours := int(age / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	case age >= time.Minute:
		minutes := int(age / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	default:
		return "less than a minute"
	}
}
