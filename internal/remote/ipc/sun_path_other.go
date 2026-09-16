//go:build !darwin && !linux

package ipc

// maxUnixSocketPathLen is the maximum usable length for a Unix domain socket
// path on platforms without a specific limit. Go's RawSockaddrUnix on Windows
// uses UNIX_PATH_MAX=108 (syscall/types_windows.go), matching Linux. Some BSDs
// use 104-byte sun_path fields; this constant does not assert runtime support
// on those platforms — it only prevents compilation failure.
const maxUnixSocketPathLen = 108
