//go:build !windows

package cli

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const treeIdentityPlatform = runtime.GOOS

func platformTreeIdentityToken(path string, info os.FileInfo) (string, error) {
	if path == "" {
		return fsq.StableTreeIdentityInfo(info)
	}
	return fsq.StableTreeIdentity(path)
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
