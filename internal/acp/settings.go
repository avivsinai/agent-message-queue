package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
)

// settingsFile is the owner's live override of the turn durations, below the
// per-user home. Identity never comes from it.
const settingsFile = "acp.settings"

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

// loadTurnTiming reads the settings file for a turn that is starting. A
// missing file, or any refused content, leaves the startup values; a refused
// file is reported once per distinct reason and never fails the turn.
func (s *Server) loadTurnTiming() turnTiming {
	t := turnTiming{s.cfg.TurnTimeout, s.cfg.PollInterval, s.cfg.HeartbeatInterval}
	err := applySettingsFile(&t)
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	s.mu.Lock()
	changed := reason != s.settingsRefused
	s.settingsRefused = reason
	s.mu.Unlock()
	if err != nil {
		if changed {
			fmt.Fprintf(os.Stderr, "amq-acp: ignoring %s, using startup values: %v\n", settingsFile, err)
		}
		return turnTiming{s.cfg.TurnTimeout, s.cfg.PollInterval, s.cfg.HeartbeatInterval}
	}
	return t
}

func applySettingsFile(t *turnTiming) error {
	dir, err := amqhome.Dir()
	if err != nil {
		return nil // no home, no file
	}
	raw, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc settingsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
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
			return fmt.Errorf("invalid %s=%q: %v", f.name, f.value, err)
		}
		*f.dst = d
	}
	return nil
}

// timing is the turn's durations: those fixed at turn start, or the startup
// values for a turn built without a start (tests, internal callers).
func (s *Server) timing(turn *turnState) turnTiming {
	if turn != nil && turn.timing != nil {
		return *turn.timing
	}
	return turnTiming{s.cfg.TurnTimeout, s.cfg.PollInterval, s.cfg.HeartbeatInterval}
}
