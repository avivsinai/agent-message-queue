//go:build unix

package launch

import (
	"fmt"
	"os"
	"syscall"
)

func executableFileIDs(_ string, info os.FileInfo) (dev, inode, volumeID, fileID uint64, err error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("executable stat is not syscall.Stat_t")
	}
	return uint64(st.Dev), uint64(st.Ino), 0, 0, nil
}
