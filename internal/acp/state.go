package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const sessionStateFilename = "cockpit-sessions.json"

type persistedSession struct {
	Thread    string `json:"thread"`
	UpdatedAt string `json:"updatedAt"`
}

type persistedSessions struct {
	Version  int                         `json:"version"`
	Channels map[string]persistedSession `json:"channels"`
}

// sessionStore persists the channel-to-thread relationship. The runtime
// session and its in-flight turn are deliberately not persisted: a process
// restart must resume the same AMQ thread without pretending a turn survived.
type sessionStore struct {
	dir      string
	channels map[string]persistedSession
	err      error
}

func newSessionStore(cfg Config) *sessionStore {
	dir := cfg.StateDir
	if dir == "" {
		dir = filepath.Join(cfg.Root, "meta", "acp")
	}
	store := &sessionStore{dir: dir, channels: make(map[string]persistedSession)}
	store.load()
	return store
}

func (s *sessionStore) path() string {
	return filepath.Join(s.dir, sessionStateFilename)
}

func (s *sessionStore) load() {
	file, info, err := fsq.OpenRegularNoFollow(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		s.err = fmt.Errorf("open ACP session state: %w", err)
		return
	}
	defer func() { _ = file.Close() }()
	if info.Size() > 1024*1024 {
		s.err = fmt.Errorf("ACP session state %s exceeds 1 MiB", s.path())
		return
	}
	var state persistedSessions
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&state); err != nil {
		s.err = fmt.Errorf("parse ACP session state %s: %w", s.path(), err)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing JSON value")
		}
		s.err = fmt.Errorf("parse ACP session state %s: %w", s.path(), err)
		return
	}
	if state.Version != 1 {
		s.err = fmt.Errorf("ACP session state %s: unsupported version %d", s.path(), state.Version)
		return
	}
	if state.Channels == nil {
		state.Channels = make(map[string]persistedSession)
	}
	for channelID, mapping := range state.Channels {
		if _, err := normalizeChannelID(channelID); err != nil || strings.TrimSpace(mapping.Thread) == "" {
			s.err = fmt.Errorf("ACP session state %s: invalid entry for channel %q", s.path(), channelID)
			return
		}
		s.channels[channelID] = mapping
	}
}

func (s *sessionStore) get(channelID string) (persistedSession, bool, error) {
	if s.err != nil {
		return persistedSession{}, false, s.err
	}
	mapping, ok := s.channels[channelID]
	return mapping, ok, nil
}

func (s *sessionStore) put(channelID, thread string, now time.Time) error {
	if s.err != nil {
		return s.err
	}
	if _, err := normalizeChannelID(channelID); err != nil {
		return err
	}
	if strings.TrimSpace(thread) == "" {
		return fmt.Errorf("ACP session thread is empty")
	}
	s.channels[channelID] = persistedSession{
		Thread:    thread,
		UpdatedAt: now.UTC().Format(time.RFC3339Nano),
	}
	return s.save()
}

func (s *sessionStore) save() error {
	state := persistedSessions{Version: 1, Channels: s.channels}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := fsq.WriteFileAtomic(s.dir, sessionStateFilename, data, 0o600); err != nil {
		return fmt.Errorf("write ACP session state: %w", err)
	}
	return nil
}

func normalizeChannelID(raw string) (string, error) {
	channelID := strings.TrimSpace(raw)
	if channelID == "" {
		return "", fmt.Errorf("ACP channel id is empty")
	}
	if len(channelID) > 512 {
		return "", fmt.Errorf("ACP channel id exceeds 512 bytes")
	}
	if strings.ContainsAny(channelID, "\r\n\x00") {
		return "", fmt.Errorf("ACP channel id contains a control character")
	}
	return channelID, nil
}

func cockpitThread(channelID string) string {
	return "cockpit/" + channelID
}

func channelIDFromMeta(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode ACP session metadata: %w", err)
	}
	channelID, found, err := findChannelID(value)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return normalizeChannelID(channelID)
}

// findChannelID derives ONE stable channel identity from the session metadata.
// Go map iteration is random, so a payload carrying more than one recognised
// value (duplicated aliases or nested candidates) must never pick a channel
// at random: a per-run pick would let a restart silently move the conversation
// to a different thread. Every recognised value is collected in deterministic
// order and accepted only when the normalized values all agree; a conflict is
// refused rather than guessed.
func findChannelID(value any) (string, bool, error) {
	var ids []string
	if err := collectChannelIDs(value, &ids); err != nil {
		return "", false, err
	}
	if len(ids) == 0 {
		return "", false, nil
	}
	normalized, err := normalizeChannelID(ids[0])
	if err != nil {
		return "", false, err
	}
	for _, id := range ids[1:] {
		n, err := normalizeChannelID(id)
		if err != nil {
			return "", false, err
		}
		if n != normalized {
			return "", false, fmt.Errorf(
				"ACP metadata carries conflicting channel ids %q and %q; send one", ids[0], id)
		}
	}
	return normalized, true, nil
}

// collectChannelIDs walks the metadata tree in deterministic order (map keys
// sorted, slices in order) and appends the string value of every recognised
// channel id key, at any nesting depth.
func collectChannelIDs(value any, ids *[]string) error {
	keys := map[string]struct{}{
		"channelId":          {},
		"channel_id":         {},
		"cockpitChannelId":   {},
		"cockpit_channel_id": {},
	}
	switch typed := value.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for key := range typed {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			child := typed[key]
			if _, recognized := keys[key]; recognized {
				channelID, ok := child.(string)
				if !ok {
					return fmt.Errorf("ACP metadata %s must be a string", key)
				}
				*ids = append(*ids, channelID)
				continue
			}
			if err := collectChannelIDs(child, ids); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := collectChannelIDs(child, ids); err != nil {
				return err
			}
		}
	}
	return nil
}
