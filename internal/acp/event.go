package acp

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	EgressConfirmed   = "confirmed"
	EgressUncertain   = "uncertain"
	DeliveryDuplicate = "already_queued"
)

var nostrEventIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The event record is a claim, not a receipt: like the mailbox steer claim
// it fixes only what a replay must not re-decide (message id, created time,
// destination, thread). It records no delivery outcome — what was delivered
// is what the recipient inbox proves at replay time, never what the first
// attempt wrote here.
type eventRecord struct {
	Schema    int    `json:"schema"`
	EventID   string `json:"event_id"`
	MessageID string `json:"message_id"`
	To        string `json:"to"`
	Thread    string `json:"thread"`
	Created   string `json:"created"`
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

// lookupEventID is the id of the Buzz event that triggered a prompt, to
// fetch it from the relay by: the Nostr id buzz-acp named in the prompt's
// _meta (eventID), else the Event ID of the prompt's one <buzz-event> block,
// which is the triggering event buzz-acp writes into the prompt text. It is
// a lookup key only: buzzevent.Fetch proves the event. "" when the prompt
// names none.
func lookupEventID(eventID, prompt string) string {
	if nostrEventIDRe.MatchString(eventID) {
		return eventID
	}
	for line := range strings.SplitSeq(buzzEvent(prompt), "\n") {
		if id, ok := strings.CutPrefix(line, "Event ID: "); ok {
			if id = strings.TrimSpace(id); nostrEventIDRe.MatchString(id) {
				return id
			}
			return ""
		}
	}
	return ""
}
