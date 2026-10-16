//go:build !darwin && !linux

package codex

import "errors"

func peerPID(int) (int, error) {
	return 0, errors.New("the socket peer pid is unavailable on this platform")
}
