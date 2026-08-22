package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	EgressConfirmed   = "confirmed"
	EgressUncertain   = "uncertain"
	DeliveryDuplicate = "already_queued"
)

var nostrEventIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type eventRecord struct {
	Schema    int    `json:"schema"`
	EventID   string `json:"event_id"`
	MessageID string `json:"message_id"`
	To        string `json:"to"`
	Thread    string `json:"thread"`
	Committed bool   `json:"committed"`
	Drained   bool   `json:"drained"`
	Started   bool   `json:"started"`
	Completed bool   `json:"completed"`
	Egress    string `json:"egress"`
}

func eventIDsFromMeta(meta json.RawMessage) ([]string, *rpcError) {
	if len(bytes.TrimSpace(meta)) == 0 {
		return nil, nil
	}
	var parsed struct {
		TriggeringEventIDs []string `json:"triggeringEventIds"`
		Nostr              struct {
			EventID  string   `json:"eventId"`
			EventIDs []string `json:"eventIds"`
		} `json:"nostr"`
		AMQ struct {
			EventID string `json:"eventId"`
		} `json:"amq"`
	}
	if err := json.Unmarshal(meta, &parsed); err != nil {
		return nil, newRPCError(codeInvalidParams, "invalid prompt _meta: %v", err)
	}

	var raw []string
	raw = append(raw, parsed.TriggeringEventIDs...)
	raw = append(raw, parsed.Nostr.EventIDs...)
	if parsed.Nostr.EventID != "" {
		raw = append(raw, parsed.Nostr.EventID)
	}
	if parsed.AMQ.EventID != "" {
		raw = append(raw, parsed.AMQ.EventID)
	}

	seen := make(map[string]struct{}, len(raw))
	ids := make([]string, 0, len(raw))
	for _, candidate := range raw {
		id := strings.ToLower(strings.TrimSpace(candidate))
		if id == "" {
			continue
		}
		if !nostrEventIDRe.MatchString(id) {
			return nil, newRPCError(codeInvalidParams, "Nostr event id %q is not 64 lowercase hex characters", candidate)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func resolveEventID(meta json.RawMessage) (string, *rpcError) {
	ids, err := eventIDsFromMeta(meta)
	if err != nil {
		return "", err
	}
	if len(ids) > 1 {
		return "", newRPCError(codeInvalidParams, "prompt carries %d Nostr event ids; one event must be one AMQ job", len(ids))
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return "", nil
}

func eventJournalPath(me, eventID string) string {
	return filepath.Join("agents", me, "outbox", "acp-events", eventID+".json")
}

func loadEventRecord(cfg Config, eventID string) (eventRecord, bool, error) {
	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return eventRecord{}, false, err
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		return eventRecord{}, false, err
	}
	defer func() { _ = root.Close() }()

	data, err := root.ReadRegularNoFollow(eventJournalPath(cfg.Me, eventID))
	if err != nil {
		if os.IsNotExist(err) {
			return eventRecord{}, false, nil
		}
		return eventRecord{}, false, err
	}
	var rec eventRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return eventRecord{}, false, err
	}
	return rec, true, nil
}

func rememberEvent(cfg Config, rec eventRecord) error {
	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	err = root.CreateExclusiveFile(eventJournalPath(cfg.Me, rec.EventID), append(data, '\n'), 0o600)
	if errors.Is(err, os.ErrExist) {
		return os.ErrExist
	}
	return err
}

func (r eventRecord) delivery() Delivery {
	return Delivery{MessageID: r.MessageID, To: r.To, Thread: r.Thread}
}
