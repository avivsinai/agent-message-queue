package turn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// settingsFile is the owner's live override of the turn durations, below the
// per-user home. Identity never comes from it.
const settingsFile = "acp.settings"

// settingsMaxBytes bounds the file read; a larger file is refused.
const settingsMaxBytes = 64 << 10

// Timing is the set of durations one turn runs with. It is fixed when the
// turn starts, so a later edit applies from the next turn.
type Timing struct {
	Timeout, Poll, Heartbeat time.Duration
}

type settingsDoc struct {
	TurnTimeout       string `json:"turn_timeout"`
	PollInterval      string `json:"poll_interval"`
	HeartbeatInterval string `json:"heartbeat_interval"`
}

// Timings reads the settings file ~/.amq/acp.settings for each turn that
// starts. It keeps the last good value and reports each distinct refusal
// once.
type Timings struct {
	name    string
	startup Timing
	// mu guards lastGood and refused.
	mu sync.Mutex
	// lastGood is the last valid settings, the startup values until a file
	// is read.
	lastGood Timing
	// refused is the last reported reason the file was refused, so a bad
	// file logs once, not on every turn.
	refused string
}

// NewTimings returns the timings of a process whose startup durations are
// startup. name prefixes its stderr lines.
func NewTimings(name string, startup Timing) *Timings {
	return &Timings{name: name, startup: startup, lastGood: startup}
}

// Load reads the settings file for a turn that is starting. A valid file
// becomes the last good value. A missing file means the startup values,
// which is how the owner reverts. A refused file keeps the last good value
// and is reported once per distinct reason; it never fails the turn.
func (t *Timings) Load() Timing {
	// The read holds no lock, so a stalled home mount never blocks another
	// turn's start behind it.
	timing, found, err := readSettingsFile(t.startup)
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case err != nil:
		if reason := err.Error(); reason != t.refused {
			t.refused = reason
			fmt.Fprintf(os.Stderr, "%s: ignoring %s, keeping the last good values: %v\n", t.name, settingsFile, err)
		}
		return t.lastGood
	case !found:
		timing = t.startup
	}
	t.refused = ""
	t.lastGood = timing
	return timing
}

func readSettingsFile(startup Timing) (Timing, bool, error) {
	t := startup
	dir, err := amqhome.Dir()
	if err != nil {
		return t, false, nil // no home, no file
	}
	f, _, err := fsq.OpenRegularNoFollow(filepath.Join(dir, settingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, settingsMaxBytes+1))
	if err != nil {
		return t, false, err
	}
	if len(raw) > settingsMaxBytes {
		return t, false, fmt.Errorf("larger than %d bytes", settingsMaxBytes)
	}
	var doc settingsDoc
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return t, false, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return t, false, errors.New("trailing data after the document")
	}
	for _, f := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"turn_timeout", doc.TurnTimeout, &t.Timeout},
		{"poll_interval", doc.PollInterval, &t.Poll},
		{"heartbeat_interval", doc.HeartbeatInterval, &t.Heartbeat},
	} {
		if f.value == "" {
			continue
		}
		d, err := time.ParseDuration(f.value)
		if err == nil && d <= 0 {
			err = errors.New("must be positive")
		}
		if err != nil {
			return startup, false, fmt.Errorf("invalid %s=%q: %v", f.name, f.value, err)
		}
		*f.dst = d
	}
	return t, true, nil
}
