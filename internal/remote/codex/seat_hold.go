package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	// seatDialogMarker is the dialog marker in a seat's agent directory,
	// which coop exec writes before Codex starts and the seat's wake reads.
	seatDialogMarker = ".wake.dialog"
	// maxSeatDialogMarkerBytes bounds the marker read.
	maxSeatDialogMarkerBytes = 64 * 1024
)

// seatStartupHeld reports whether the AMQ seat that carries thread on the
// daemon at socket still holds input at Codex's startup screens: coop exec
// recorded the seat's root and handle for thread, and the seat has a Codex
// Startup marker that only its dialog hook releases. The seat's wake holds
// its doorbell on the same marker. A daemon seat starts only in a directory
// Codex trusts, so its marker never waits on a trust record. A marker that
// cannot be read holds, as it does for the wake. A thread AMQ did not
// record, or whose record is revoked or damaged, holds nothing.
func seatStartupHeld(socket, thread string) bool {
	if socket == "" {
		return false
	}
	rec, err := codexidentity.Lookup(thread, filepath.Dir(filepath.Dir(socket)))
	if err != nil {
		return false
	}
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(fsq.AgentBase(rec.Root, rec.Me), seatDialogMarker), maxSeatDialogMarkerBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	var marker struct {
		Event   string `json:"event"`
		Harness string `json:"harness"`
		Kind    string `json:"kind"`
	}
	if json.Unmarshal(raw, &marker) != nil {
		return true
	}
	return marker.Event == "Startup" && marker.Harness == "codex" && marker.Kind != ""
}
