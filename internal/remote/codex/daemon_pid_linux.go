//go:build linux

package codex

import "golang.org/x/sys/unix"

func peerPID(fd int) (int, error) {
	peer, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(peer.Pid), nil
}
