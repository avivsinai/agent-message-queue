//go:build !windows

package cli

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

const treeIdentityPlatform = runtime.GOOS

func platformTreeIdentityToken(_ string, info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("filesystem identity unavailable on %s", runtime.GOOS)
	}
	return fmt.Sprintf("v1:%s:%x:%x", treeIdentityPlatform, uint64(stat.Dev), uint64(stat.Ino)), nil
}

func validPlatformTreeIdentityToken(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != treeIdentityPlatform {
		return false
	}
	if _, err := strconv.ParseUint(parts[2], 16, 64); err != nil {
		return false
	}
	_, err := strconv.ParseUint(parts[3], 16, 64)
	return err == nil
}
