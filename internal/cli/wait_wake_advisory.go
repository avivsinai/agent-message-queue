package cli

// noteLiveWakeBeforeBlockingWait prints one stderr note when a blocking
// arrival wait (watch, monitor) starts while a live injecting wake already
// notifies this agent's terminal. Two arrival mechanisms for one agent are
// redundant at best; at worst the harness blocks on the wait and the wake's
// doorbell queues behind it (issue #717). A notify-only wake (mode none) is
// the supervisor recipe, where monitor is the sole consumer, so it stays
// silent. Advisory only: the wait proceeds either way.
func noteLiveWakeBeforeBlockingWait(command, root, me string) {
	inspection := inspectWakeLock(root, me)
	if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		return
	}
	mode := inspection.Lock.WakeMode
	if mode == wakeInjectModeNone {
		return
	}
	if mode == "" {
		mode = "legacy"
	}
	_ = writeStderr(
		"note: a live wake (pid %d, mode %s) already notifies this terminal; %s is redundant here. Receive with amq drain --include-body.\n",
		inspection.PID, mode, command,
	)
}
