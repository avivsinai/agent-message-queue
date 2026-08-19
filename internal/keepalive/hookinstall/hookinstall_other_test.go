//go:build !unix

package hookinstall

func processRunning(int) bool { return false }

func killPID(int) {}
