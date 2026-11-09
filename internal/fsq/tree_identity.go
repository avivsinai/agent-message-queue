package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// SamePersistedTreeIdentity reports whether stored, a token a consumer wrote
// to disk, names the same file as live, the token computed now for the same
// canonical path. macOS assigns st_dev when it mounts a volume, so a token
// stored before a reboot or OS update can carry another device number for
// the same file; the platform and file ID must still match. Process-lifetime
// pins compare tokens exactly instead. The caller must already have matched
// the canonical path: across devices a file ID alone names no file.
func SamePersistedTreeIdentity(stored, live string) bool {
	if stored == live {
		return stored != ""
	}
	s, sok := splitIdentityToken(stored)
	l, lok := splitIdentityToken(live)
	return sok && lok && s[1] == l[1] && s[3] == l[3]
}

func splitIdentityToken(token string) ([]string, bool) {
	parts := strings.Split(token, ":")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] == "" {
		return nil, false
	}
	for _, field := range parts[2:] {
		if _, err := strconv.ParseUint(field, 16, 64); err != nil {
			return nil, false
		}
	}
	return parts, true
}
