package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

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

// seatStartupHeld reports whether the AMQ seat that carries thread still
// holds input at Codex's startup screens: coop exec recorded the seat's
// root and handle for thread under this process's Codex home, and the seat
// has a Codex
// Startup marker that only its dialog hook releases. The seat's wake holds
// its doorbell on the same marker. A daemon seat starts only in a directory
// Codex trusts, so its marker never waits on a trust record. A marker that
// cannot be read holds, as it does for the wake. A thread AMQ did not
// record, or whose record is revoked or damaged, holds nothing.
func seatStartupHeld(thread string) bool {
	codexHome, err := codexHomeDir()
	if err != nil {
		return false
	}
	rec, err := codexidentity.Lookup(thread, codexHome)
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

// codexHomeDir is the Codex home coop exec records a thread under:
// $CODEX_HOME, or ~/.codex, made absolute. The daemon socket's own path
// does not name it: the socket can be a symlink target in another
// directory.
func codexHomeDir() (string, error) {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		codexHome = filepath.Join(home, ".codex")
	}
	return filepath.Abs(codexHome)
}
