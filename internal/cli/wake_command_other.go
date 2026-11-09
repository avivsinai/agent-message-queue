//go:build !(darwin || linux)

package cli

// wakeCommands is the owner command queue, which only a unix wake types.
type wakeCommands struct{}

// ownerCommandMaySubmit reports whether the owner command being typed may
// get its Enter; there is none on this platform.
func ownerCommandMaySubmit(*wakeConfig) bool { return false }
