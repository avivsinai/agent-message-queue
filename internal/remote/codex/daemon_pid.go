package codex

import (
	"fmt"
	"net"
	"time"
)

// DaemonPID is the pid of the process serving the app-server daemon socket
// at socketPath, as the kernel reports the connected socket's peer. It names
// the daemon instance: a restarted daemon serves the same path from another
// process.
func DaemonPID(socketPath string) (int, error) {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var peerErr error
	if err := raw.Control(func(fd uintptr) { pid, peerErr = peerPID(int(fd)) }); err != nil {
		return 0, err
	}
	if peerErr != nil {
		return 0, peerErr
	}
	if pid <= 0 {
		return 0, fmt.Errorf("app-server socket %s reports no peer pid", socketPath)
	}
	return pid, nil
}
