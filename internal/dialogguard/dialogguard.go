// Package dialogguard owns the wake dialog guard hooks: the marker file a
// hook keeps in an agent's mailbox while a harness dialog is open, the shell
// command every harness's hook runs, and the Claude Code guard in one
// settings file, as amq doctor and amq-remote doctor read and install it.
package dialogguard

import "github.com/avivsinai/agent-message-queue/internal/update"

const (
	// FileName is the dialog marker in <root>/agents/<me>/.
	FileName = ".wake.dialog"
	// HookMarker starts every hook command the installer writes; uninstall
	// and doctor find AMQ's entries by it.
	HookMarker = "AMQ_DIALOG_HOOK=1 "
)

// Claude dialog guard states in one settings file (ClaudeAt).
const (
	// Unsupported: this platform has no wake, so no guard.
	Unsupported = "unsupported"
	OK          = "ok"
	Missing     = "missing"
	Incomplete  = "incomplete" // some events, an unknown command, or a missing amq binary
	// Stale: every hook holds, but an older form whose open names no host,
	// so a hold does not close when Claude Code exits.
	Stale      = "stale"
	Disabled   = "disabled" // installed, but the file sets disableAllHooks
	Unreadable = "unreadable"
)

// Check is the guard's verdict on one settings file: its state and, when it
// is not OK, why and the remedy.
type Check struct {
	State   string
	Message string
}

// EntriesSince is the first AMQ release whose wake reads .wake.dialog as a
// directory of entries. An older wake reads it only as a regular file and
// holds input while it is a directory.
const EntriesSince = "0.128.0"

// WakeReadsEntries reports whether a wake of AMQ version reads the
// directory of dialog entries; known is false when version is not a release
// version (empty, "unknown", or a local build name).
func WakeReadsEntries(version string) (reads, known bool) {
	cmp, ok := update.CompareVersions(version, EntriesSince)
	return ok && cmp >= 0, ok
}
