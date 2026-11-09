//go:build !(darwin || linux)

package cli

// wakeCommands is the owner command queue, which only a unix wake types.
type wakeCommands struct{}
