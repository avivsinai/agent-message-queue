//go:build unix

package codexlaunch

import (
	"os"
	"syscall"
)

// sharedDirectoryPrivate requires the user-owned 0700 directory Codex keeps
// its private daemon sockets in.
func sharedDirectoryPrivate(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
