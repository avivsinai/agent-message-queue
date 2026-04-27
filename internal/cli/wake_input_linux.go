//go:build linux

package cli

import "golang.org/x/sys/unix"

const ttyInputQueueRequest = unix.TIOCINQ
