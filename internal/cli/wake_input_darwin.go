//go:build darwin

package cli

const ttyInputQueueRequest = 0x4004667f // FIONREAD: _IOR('f', 127, int)
