//go:build darwin

package ipc

// maxUnixSocketPathLen is the maximum usable length for a Unix domain socket
// path on macOS (sun_path is 104 bytes). A path at or beyond this length fails
// bind with a bare EINVAL (611.23).
const maxUnixSocketPathLen = 104
