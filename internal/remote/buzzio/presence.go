package buzzio

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"fiatjaf.com/nostr"
)

// Kinds the presence surface publishes (relay design §6). Both are
// agent-authored and replaceable; Buzz Desktop reads the latest of each.
const (
	KindProfile      = 0
	KindAgentProfile = 10100
	// KindManagedAgent is the owner-authored policy coordinate (d = body)
	// Desktop requires to list the body as owned; the body never signs it.
	KindManagedAgent = 30177
)

// PolicyFor reports whether evt is a 30177 coordinate for body that the
// pinned Desktop parser accepts (buzz a929532 desktop managed_agents/
// agent_events.rs:37-60 ManagedAgentEventContent): name is a string,
// parallelism a u32, respond_to one of its wire values, and
// respond_to_allowlist a list of strings. Desktop drops a policy that fails
// that parse (nostr_convert/agent_directory.rs:89-98), so AMQ does too
// (codex #867 r1).
func PolicyFor(evt nostr.Event, body string) bool {
	if evt.Kind != KindManagedAgent || tagValue(evt, "d") != body {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(evt.Content), &fields) != nil {
		return false
	}
	var c struct {
		Name        *string `json:"name"`
		Parallelism *uint32 `json:"parallelism"`
		RespondTo   *string `json:"respond_to"`
	}
	if json.Unmarshal([]byte(evt.Content), &c) != nil || c.Name == nil || c.Parallelism == nil || c.RespondTo == nil {
		return false
	}
	switch *c.RespondTo {
	case "anyone", "owner-only", "allowlist":
	default:
		return false
	}
	// Option<String> fields: absent, null, or a string (codex #867 r2).
	for _, k := range []string{"persona_id", "system_prompt", "model", "provider", "persona_source_version"} {
		if raw, ok := fields[k]; ok && !isNullOr(raw, '"') {
			return false
		}
	}
	// Vec<String>: absent, or an array of strings with no null element.
	if raw, ok := fields["respond_to_allowlist"]; ok {
		var list []*string
		if json.Unmarshal(raw, &list) != nil || isNullOr(raw, 0) {
			return false
		}
		for _, v := range list {
			if v == nil {
				return false
			}
		}
	}
	return true
}

// isNullOr reports whether raw is JSON null or, when first is not 0, a value
// whose first byte is first ('"' for a string).
func isNullOr(raw json.RawMessage, first byte) bool {
	v := bytes.TrimSpace(raw)
	if bytes.Equal(v, []byte("null")) {
		return true
	}
	return first != 0 && len(v) > 0 && v[0] == first
}

// Presence statuses Buzz Desktop accepts as explicit runtime evidence
// (buzz desktop nostr_convert/agent_directory.rs:59); anything else reads
// "unknown".
const (
	StatusOnline  = "online"
	StatusAway    = "away"
	StatusOffline = "offline"
)

// Presence builds one body's kind 0 profile and kind 10100 status events.
// Each is signed under the owner's grant for its kind, carries that grant
// as its one NIP-OA tag, and is dated strictly after the previous event of
// the same kind, so a replaceable record is never backdated behind a newer
// one.
type Presence struct {
	signer Signer
	name   string

	mu   sync.Mutex
	last map[nostr.Kind]int64
}

// NewPresence returns the presence builder for a body. name is the safe
// display name the operator chose; it is published in clear text.
func NewPresence(signer Signer, name string) *Presence {
	return &Presence{signer: signer, name: name, last: map[nostr.Kind]int64{}}
}

// Profile is the body's kind 0. Desktop names the owner only from its one
// verified NIP-OA tag, whose conditions must admit kind 0.
func (p *Presence) Profile(now time.Time) (nostr.Event, error) {
	content, _ := json.Marshal(map[string]any{"name": p.name, "display_name": p.name, "bot": true})
	return p.build(KindProfile, string(content), now)
}

// Status is the body's kind 10100 with an explicit status. AMQ execution
// capabilities stay out of it: /inspect is the exact capability view.
func (p *Presence) Status(status string, now time.Time) (nostr.Event, error) {
	content, _ := json.Marshal(map[string]any{
		"name": p.name, "agent_type": "agent",
		"channels": []string{}, "channel_ids": []string{}, "capabilities": []string{},
		"status": status,
	})
	return p.build(KindAgentProfile, string(content), now)
}

func (p *Presence) build(kind nostr.Kind, content string, now time.Time) (nostr.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := now.Unix()
	if at <= p.last[kind] {
		at = p.last[kind] + 1
	}
	evt := nostr.Event{CreatedAt: nostr.Timestamp(at), Kind: kind, Content: content, Tags: nostr.Tags{}}
	if err := p.signer.sign(&evt); err != nil {
		return nostr.Event{}, err
	}
	p.last[kind] = at
	return evt, nil
}
