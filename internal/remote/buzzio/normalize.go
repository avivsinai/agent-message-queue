package buzzio

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

// Command ops an owner DM can carry.
const (
	OpSubmit      = "submit"
	OpInspect     = "inspect"
	OpStatus      = "status"
	OpCancel      = "cancel"
	OpUnsupported = "unsupported"
)

// MutationWindow is how long after its signed created_at an owner command
// that changes work may still be dispatched (relay design §4, ratified: two
// minutes from the authenticated message, never from receipt or wake).
const MutationWindow = 2 * time.Minute

// maxFutureSkew bounds how far in the future a command may be dated.
const maxFutureSkew = 30 * time.Second

// Binding is one shared session's DM surface: who may command it, and in
// which private channel.
type Binding struct {
	Owner     string // owner pubkey, 64 lowercase hex
	Body      string // body pubkey, 64 lowercase hex
	Channel   string // the owner DM channel id (the h tag)
	Target    string // the one shared adapter target
	RelayHost string // relay identity, part of the ingress key
	// Mentions are the opted-in channels where an owner message that
	// mentions the body (p tag) submits; output still goes to Channel.
	Mentions map[string]bool
}

// Normalized is an owner command reduced to what the endpoint needs.
type Normalized struct {
	Op        string
	Text      string // the prompt, for submit
	Ref       string // the request ref, for status/cancel
	RequestID string // deterministic per signed event, for submit
	NotAfter  time.Time
}

// ErrNotForUs is an event this edge must ignore: not from the owner, not in
// the bound channel, or not a DM message.
var ErrNotForUs = errors.New("event is not an owner command for this share")

// ErrStale is a mutating command signed too long ago, or dated in the
// future; it never executes, even after a reconnect.
var ErrStale = errors.New("command is outside its admission window")

// Normalize turns one verified owner event into a command. Authority is the
// signature and the pubkey only: display names, mentions, channel
// membership and claimed refs are never authority. The event's signature
// was verified by the relay subscription before it got here.
func Normalize(evt nostr.Event, b Binding, now time.Time) (Normalized, error) {
	if evt.Kind != 9 || evt.PubKey.Hex() != b.Owner || tagValue(evt, "h") != b.Channel || b.Channel == "" {
		return Normalized{}, ErrNotForUs
	}
	text := strings.TrimSpace(evt.Content)
	created := time.Unix(int64(evt.CreatedAt), 0)
	n := Normalized{}
	switch {
	case text == "/inspect":
		n.Op = OpInspect
	case strings.HasPrefix(text, "/status"):
		n.Op, n.Ref = OpStatus, strings.TrimSpace(strings.TrimPrefix(text, "/status"))
	case strings.HasPrefix(text, "/cancel"):
		n.Op, n.Ref = OpCancel, strings.TrimSpace(strings.TrimPrefix(text, "/cancel"))
		if n.Ref == "" {
			return Normalized{}, fmt.Errorf("/cancel needs a request ref")
		}
	case strings.HasPrefix(text, "/"):
		n.Op = OpUnsupported // answered as unsupported, never sent as a prompt
	case text == "":
		return Normalized{}, ErrNotForUs
	default:
		n.Op, n.Text = OpSubmit, text
		n.RequestID = requestIDFor(b, evt.ID.Hex())
	}
	if n.Op == OpSubmit || n.Op == OpCancel {
		if created.After(now.Add(maxFutureSkew)) || now.Sub(created) > MutationWindow {
			return Normalized{}, ErrStale
		}
		n.NotAfter = created.Add(MutationWindow)
	}
	return n, nil
}

// NormalizeMention turns one verified owner event in an opted-in mention
// channel into a submit. It must carry a p tag for the body, and only plain
// prompts submit: slash commands belong in the DM and are answered as
// unsupported. Leading NIP-27 profile references (the addressing, such as
// "nostr:npub1… fix the build") are removed; the rest of the text is the
// prompt, unchanged.
func NormalizeMention(evt nostr.Event, b Binding, now time.Time) (Normalized, error) {
	ch := tagValue(evt, "h")
	if evt.Kind != 9 || evt.PubKey.Hex() != b.Owner || ch == "" || ch == b.Channel || !b.Mentions[ch] || !hasP(evt, b.Body) {
		return Normalized{}, ErrNotForUs
	}
	text := stripLeadingMentions(evt.Content)
	switch {
	case text == "":
		return Normalized{}, ErrNotForUs
	case strings.HasPrefix(text, "/"):
		return Normalized{Op: OpUnsupported}, nil
	}
	created := time.Unix(int64(evt.CreatedAt), 0)
	if created.After(now.Add(maxFutureSkew)) || now.Sub(created) > MutationWindow {
		return Normalized{}, ErrStale
	}
	return Normalized{Op: OpSubmit, Text: text, RequestID: requestIDFor(b, evt.ID.Hex()), NotAfter: created.Add(MutationWindow)}, nil
}

// stripLeadingMentions drops leading whitespace-separated nostr:npub1… and
// nostr:nprofile1… tokens.
func stripLeadingMentions(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "nostr:npub1") || strings.HasPrefix(s, "nostr:nprofile1") {
		i := strings.IndexAny(s, " \t\n")
		if i < 0 {
			return ""
		}
		s = strings.TrimSpace(s[i:])
	}
	return s
}

func hasP(evt nostr.Event, pub string) bool {
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "p" && t[1] == pub {
			return true
		}
	}
	return false
}

// requestIDFor derives the request UUID from the relay, body, owner and
// signed event id, so every import of one event addresses one request.
func requestIDFor(b Binding, eventID string) string {
	sum := sha256.Sum256([]byte("amq-remote/buzz/request\x00" + b.RelayHost + "\x00" + b.Body + "\x00" + b.Owner + "\x00" + eventID))
	u := sum[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// tagValue returns the first value of the named single-letter tag.
func tagValue(evt nostr.Event, name string) string {
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1]
		}
	}
	return ""
}
