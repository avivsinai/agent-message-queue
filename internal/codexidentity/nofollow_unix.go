//go:build !windows

package codexidentity

import "syscall"

const openNoFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
