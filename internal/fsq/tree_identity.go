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

// StableFileIdentityInfo returns the opaque identity for an already opened
// regular-file snapshot.
func StableFileIdentityInfo(info os.FileInfo) (string, error) {
	if info == nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("file identity requires a regular file snapshot")
	}
	return platformStableTreeIdentity("", info)
}

// StableTreeIdentityInfo returns the same token from an already authorized
// filesystem snapshot.
func StableTreeIdentityInfo(info os.FileInfo) (string, error) {
	if info == nil || !info.IsDir() {
		return "", fmt.Errorf("tree identity requires a directory snapshot")
	}
	return platformStableTreeIdentity("", info)
}

// StableFileIdentity returns an opaque physical identity for one regular file.
// It is used to pin executable targets across a delayed launch boundary.
func StableFileIdentity(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty file path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("file identity requires a regular file: %s", abs)
	}
	return platformStableTreeIdentity(abs, info)
}
