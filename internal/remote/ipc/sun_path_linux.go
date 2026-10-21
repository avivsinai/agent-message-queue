//go:build linux

package ipc

// maxUnixSocketPathLen is the maximum usable length for a Unix domain socket
// path on Linux (sun_path is 108 bytes). A path at or beyond this length fails
// bind with a bare EINVAL.
const maxUnixSocketPathLen = 108
