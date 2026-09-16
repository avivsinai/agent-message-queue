//go:build !darwin && !linux

package ipc

// maxUnixSocketPathLen is the conservative maximum length for a Unix domain
// socket path on platforms without a specific limit (Windows, FreeBSD, etc.).
// Unix domain sockets are not typically used on Windows, but the constant
// must be defined for compilation. 104 is the most restrictive common limit.
const maxUnixSocketPathLen = 104
