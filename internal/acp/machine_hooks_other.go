//go:build !unix

package acp

import "os"

// fileIdentity is "?": this platform gives no inode, so a file's version is
// its size and mtime.
func fileIdentity(os.FileInfo) string { return "?" }
