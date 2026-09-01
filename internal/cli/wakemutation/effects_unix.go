//go:build darwin || linux

package wakemutation

import (
	"fmt"
	"os"
)

type UnlinkAtFunc func(dirfd int, name string, flags int) error

func (lease *Lease) UnlinkAt(dirfd int, name string, flags int) error {
	return lease.UnlinkAtWith(unixUnlinkAt, dirfd, name, flags)
}

func (lease *Lease) UnlinkAtWith(
	unlink UnlinkAtFunc,
	dirfd int,
	name string,
	flags int,
) error {
	if unlink == nil {
		return fmt.Errorf("wake unlink capability is missing")
	}
	return lease.withEffect(func() error {
		return unlink(dirfd, name, flags)
	})
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return processSignalAlive(process)
}

func (lease *Lease) KillProcess(process *os.Process) error {
	if process == nil {
		return fmt.Errorf("process is missing")
	}
	return lease.withEffect(func() error { return process.Kill() })
}
