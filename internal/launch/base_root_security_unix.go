//go:build !windows

package launch

import (
	"fmt"
	"os"
	"syscall"
)

func validateExactAMQRCFileInfo(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf(".amqrc ownership metadata is unavailable")
	}
	if uint32(stat.Uid) != uint32(os.Geteuid()) {
		return fmt.Errorf(".amqrc is owned by uid %d, want euid %d", stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(".amqrc is group/world-writable")
	}
	return nil
}
