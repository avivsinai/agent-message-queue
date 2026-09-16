//go:build !darwin && !linux

package ipc

// maxUnixSocketPathLen is the maximum usable length for a Unix domain socket
// path on platforms without a specific limit. Go's RawSockaddrUnix on Windows
// uses UNIX_PATH_MAX=108 (syscall/types_windows.go), matching Linux. FreeBSD
// and other BSDs also use 104–108 bytes; 108 is the safe upper bound.
const maxUnixSocketPathLen = 108
