package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StableTreeIdentity returns the opaque physical identity used to bind
// authority to one project directory. Callers must not parse the token.
func StableTreeIdentity(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty tree path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("tree path is not a directory: %s", abs)
	}
	return platformStableTreeIdentity(abs, info)
}

// StableTreeIdentityInfo returns the same token from an already authorized
// filesystem snapshot.
func StableTreeIdentityInfo(info os.FileInfo) (string, error) {
	if info == nil || !info.IsDir() {
		return "", fmt.Errorf("tree identity requires a directory snapshot")
	}
	return platformStableTreeIdentity("", info)
}
