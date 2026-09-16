//go:build linux

package ipc

// maxUnixSocketPathLen is the maximum usable length for a Unix domain socket
// path on Linux (sun_path is 108 bytes). A path at or beyond this length fails
// bind with a bare EINVAL (611.23).
const maxUnixSocketPathLen = 108
