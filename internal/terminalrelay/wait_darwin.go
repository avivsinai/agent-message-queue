package terminalrelay

import "syscall"

// Darwin encodes continued status with SIGCONT, not SIGSTOP. The generic
// syscall BSD decoder does not match Darwin's sys/wait.h for these signals.
func waitStopped(status syscall.WaitStatus) bool {
	return status&0x7f == 0x7f && syscall.Signal(status>>8) != syscall.SIGCONT
}

func waitContinued(status syscall.WaitStatus) bool {
	return status&0x7f == 0x7f && syscall.Signal(status>>8) == syscall.SIGCONT
}
