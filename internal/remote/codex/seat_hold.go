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
	// seatStartupBusy and seatHomeUnknownBusy are the busy reasons the
	// attachment gives while it holds a turn for a seat.
	seatStartupBusy     = "the Codex seat on this thread has not passed its startup screens yet"
	seatHomeUnknownBusy = "AMQ recorded this thread for a Codex seat, but the daemon did not report a rollout path that names the seat's Codex home, so AMQ cannot check the seat's startup screens"
)

// seatStartupHeld reports whether the AMQ seat that carries thread still
// holds input at Codex's startup screens, and the busy reason when it does.
// codexHome is the daemon's Codex home, proven by the thread's rollout path
// (codexHomeFromRollout), and coop exec must have recorded the seat's root
// and handle for thread under that same home. The seat holds while it has
// a Codex Startup marker that only its dialog hook releases; the seat's
// wake holds its doorbell on the same marker. A daemon seat starts only in a
// directory Codex trusts, so its marker never waits on a trust record. A
// marker that cannot be read holds, as it does for the wake. A thread AMQ
// did not record, or whose record is revoked, damaged or names another
// home, holds nothing.
//
// Without a proven home the marker cannot be found, so a thread that still
// carries an AMQ seat identity holds until the attachment is replaced. When
// the identity store cannot be read, the thread holds nothing: that is the
// limit of this check.
func seatStartupHeld(thread, codexHome string) (bool, string) {
	if codexHome == "" {
		bindings, err := codexidentity.Bindings()
		if err != nil {
			return false, ""
		}
		for _, b := range bindings {
			if b.Thread == thread && b.Revoked.IsZero() {
				return true, seatHomeUnknownBusy
			}
		}
		return false, ""
	}
	rec, err := codexidentity.Lookup(thread, codexHome)
	if err != nil {
		return false, ""
	}
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(fsq.AgentBase(rec.Root, rec.Me), seatDialogMarker), maxSeatDialogMarkerBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, ""
	}
	if err != nil {
		return true, seatStartupBusy
	}
	var marker struct {
		Event   string `json:"event"`
		Harness string `json:"harness"`
		Kind    string `json:"kind"`
	}
	if json.Unmarshal(raw, &marker) != nil {
		return true, seatStartupBusy
	}
	if marker.Event == "Startup" && marker.Harness == "codex" && marker.Kind != "" {
		return true, seatStartupBusy
	}
	return false, ""
}

// codexHomeFromRollout is the Codex home of the daemon that reported path as
// thread's rollout file, or "" when path does not have the shape Codex gives
// it: <home>/sessions/YYYY/MM/DD/<file naming the thread>, with home
// canonical (rollout/src/recorder.rs precompute_new_rollout_path and
// utils/home-dir find_codex_home at rust-v0.164.0). Neither amq-remote's own
// environment nor the daemon socket's directory names the seat's home: the
// service can run with another CODEX_HOME, and the socket can be a symlink
// target elsewhere.
func codexHomeFromRollout(path, thread string) string {
	if path == "" || !filepath.IsAbs(path) || !strings.Contains(filepath.Base(path), thread) {
		return ""
	}
	day := filepath.Dir(path)
	month := filepath.Dir(day)
	year := filepath.Dir(month)
	sessions := filepath.Dir(year)
	if !digits(filepath.Base(day), 2) || !digits(filepath.Base(month), 2) || !digits(filepath.Base(year), 4) || filepath.Base(sessions) != "sessions" {
		return ""
	}
	return filepath.Dir(sessions)
}

// digits reports whether s is exactly n ASCII digits.
func digits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
