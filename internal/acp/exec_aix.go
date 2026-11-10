//go:build aix

package acp

// canExecute is false on AIX: golang.org/x/sys/unix has no AT_EACCESS there
// to check the effective ids, and the bundled CLI it gates ships only inside
// a macOS Buzz.app.
func canExecute(string) bool { return false }
