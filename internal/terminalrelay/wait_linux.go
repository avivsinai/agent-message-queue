package terminalrelay

import "syscall"

func waitStopped(status syscall.WaitStatus) bool   { return status.Stopped() }
func waitContinued(status syscall.WaitStatus) bool { return status.Continued() }
