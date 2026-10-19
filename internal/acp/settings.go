package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// settingsFile is the owner's live override of the turn durations, below the
// per-user home. Identity never comes from it.
const settingsFile = "acp.settings"

// settingsMaxBytes bounds the file read; a larger file is refused.
const settingsMaxBytes = 64 << 10

// turnTiming is the set of durations one turn runs with. It is fixed when the
// turn starts, so a later edit applies from the next turn.
type turnTiming struct {
	timeout, poll, heartbeat time.Duration
}

type settingsDoc struct {
	TurnTimeout       string `json:"turn_timeout"`
	PollInterval      string `json:"poll_interval"`
	HeartbeatInterval string `json:"heartbeat_interval"`
}

// loadTurnTiming reads the settings file for a turn that is starting. A valid
// file becomes the last good value. A missing file means the startup values,
// which is how the owner reverts. A refused file keeps the last good value and
// is reported once per distinct reason; it never fails the turn.
func (s *Server) loadTurnTiming() turnTiming {
	startup := turnTiming{s.cfg.TurnTimeout, s.cfg.PollInterval, s.cfg.HeartbeatInterval}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastGood == nil {
		s.lastGood = &startup
	}
	t, found, err := readSettingsFile(startup)
	switch {
	case err != nil:
		if reason := err.Error(); reason != s.settingsRefused {
			s.settingsRefused = reason
			fmt.Fprintf(os.Stderr, "amq-acp: ignoring %s, keeping the last good values: %v\n", settingsFile, err)
		}
		return *s.lastGood
	case !found:
		t = startup
	}
	s.settingsRefused = ""
	s.lastGood = &t
	return t
}

func readSettingsFile(startup turnTiming) (turnTiming, bool, error) {
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
	if err := json.Unmarshal(raw, &doc); err != nil {
		return t, false, err
	}
	for _, f := range []struct {
		name, value string
		dst         *time.Duration
	}{
		{"turn_timeout", doc.TurnTimeout, &t.timeout},
		{"poll_interval", doc.PollInterval, &t.poll},
		{"heartbeat_interval", doc.HeartbeatInterval, &t.heartbeat},
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

// timing is the turn's durations: those fixed at turn start, or the startup
// values for a turn built without a start (tests, internal callers).
func (s *Server) timing(turn *turnState) turnTiming {
	if turn != nil && turn.timing != nil {
		return *turn.timing
	}
	return turnTiming{s.cfg.TurnTimeout, s.cfg.PollInterval, s.cfg.HeartbeatInterval}
}
