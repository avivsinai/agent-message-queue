//go:build !unix

package codexlaunch

// sharedDirectoryPrivate cannot prove the private socket directory here, so
// no socket is the verified local daemon.
func sharedDirectoryPrivate(string) bool { return false }
