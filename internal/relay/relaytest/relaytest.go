// Package relaytest is an in-process Nostr relay for AMQ relay tests.
package relaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// Start serves a Relay expecting the given body and tag, and returns it with
// its ws:// URL. Close the server when done.
func Start(bodyPubHex string, tag []string) (*Relay, *httptest.Server, string) {
	r := &Relay{body: bodyPubHex, tag: tag}
	srv := httptest.NewServer(r)
	return r, srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// Relay is an in-process NIP-42 relay for tests: it challenges, verifies the
// AUTH event the way the relay design pins it (kind 22242, relay and
// challenge tags, exactly one NIP-OA tag that verifies for the body, valid
// signature), and acknowledges with OK.
type Relay struct {
	body string   // expected body pubkey hex
	tag  []string // expected NIP-OA tag
	// AuthOK counts accepted AUTH events.
	AuthOK atomic.Int32
	// DropOnce closes the first authenticated connection, to exercise
	// reconnect.
	DropOnce atomic.Bool
	// RejectKind, when non-zero, refuses events of this kind instead of
	// storing them. Rejected counts their refused publishes.
	RejectKind atomic.Uint64
	// DropKind, when non-zero, closes a connection after the next publish of
	// that event kind, before an OK or the event is stored, so the outcome is
	// unknown.
	DropKind atomic.Uint64
	Rejected atomic.Int32
	// BeforePublish, when set before the first connection, sees every
	// publish the relay receives, also one that the relay then drops, before
	// it answers.
	BeforePublish func(nostr.Event)
	// Accepted, when set before the first connection, runs on each publish
	// the relay accepts, before its OK is sent, so a test can store the
	// group state the event changes as a relay does.
	Accepted func(nostr.Event)
	// BeforeReq, when set before the first connection, runs on each REQ
	// before its stored events are sent, so a test can store an event
	// between two reads.
	BeforeReq func(filters []nostr.Filter)
	// Self, when set before the first request, is the relay's own public
	// key (hex) that its NIP-11 document names.
	Self string

	mu        sync.Mutex
	stored    []nostr.Event
	attempted []nostr.Event
	live      map[*liveSub]struct{}
}

type liveSub struct {
	id      string
	filters []nostr.Filter
	send    func([]byte)
}

// Inject stores an event and fans it out to open subscriptions without
// checking it, so tests can deliver a forged event and see the client
// refuse it.
func (r *Relay) Inject(evt nostr.Event) { r.store(evt) }

// Attempts returns every publish the relay received so far, in order,
// also those it refused or dropped. A resend of one event is in it again.
func (r *Relay) Attempts() []nostr.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]nostr.Event(nil), r.attempted...)
}

// Events returns the events accepted so far.
func (r *Relay) Events() []nostr.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]nostr.Event(nil), r.stored...)
}

func (r *Relay) store(evt nostr.Event) {
	r.mu.Lock()
	r.stored = append(r.stored, evt)
	subs := make([]*liveSub, 0, len(r.live))
	for s := range r.live {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	for _, s := range subs {
		for _, f := range s.filters {
			if f.Matches(evt) {
				id := s.id
				frame, _ := nostr.EventEnvelope{SubscriptionID: &id, Event: evt}.MarshalJSON()
				s.send(frame)
				break
			}
		}
	}
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.Self != "" && req.Header.Get("Accept") == "application/nostr+json" {
		w.Header().Set("Content-Type", "application/nostr+json")
		_ = json.NewEncoder(w).Encode(struct {
			Self string `json:"self"`
		}{r.Self})
		return
	}
	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ctx := req.Context()
	var writeMu sync.Mutex
	send := func(frame []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.Write(ctx, websocket.MessageText, frame)
	}
	challenge := fmt.Sprintf("chal-%d", time.Now().UnixNano())
	chJSON, _ := nostr.AuthEnvelope{Challenge: &challenge}.MarshalJSON()
	if ws.Write(ctx, websocket.MessageText, chJSON) != nil {
		return
	}
	authed := false
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		env, err := nostr.ParseMessage(string(data))
		if err != nil {
			continue
		}
		switch e := env.(type) {
		case *nostr.AuthEnvelope:
			reason := r.checkAuth(e.Event, challenge, "ws://"+req.Host)
			ok, _ := nostr.OKEnvelope{EventID: e.Event.ID, OK: reason == "", Reason: reason}.MarshalJSON()
			send(ok)
			if reason == "" {
				authed = true
				r.AuthOK.Add(1)
				if r.DropOnce.CompareAndSwap(true, false) {
					return // drop the first authenticated connection
				}
			}
		case *nostr.EventEnvelope:
			r.mu.Lock()
			r.attempted = append(r.attempted, e.Event)
			r.mu.Unlock()
			if r.BeforePublish != nil {
				r.BeforePublish(e.Event)
			}
			if drop := r.DropKind.Load(); drop != 0 && drop == uint64(e.Kind) && r.DropKind.CompareAndSwap(drop, 0) {
				return
			}
			reason := ""
			if !authed {
				reason = "auth-required: authenticate first"
			} else if !e.VerifySignature() {
				reason = "invalid: bad signature"
			} else if r.RejectKind.Load() != 0 && uint64(e.Kind) == r.RejectKind.Load() {
				r.Rejected.Add(1)
				reason = "blocked: test relay refused the event kind"
			}
			if reason == "" && r.Accepted != nil {
				r.Accepted(e.Event)
			}
			ok, _ := nostr.OKEnvelope{EventID: e.Event.ID, OK: reason == "", Reason: reason}.MarshalJSON()
			send(ok)
			if reason == "" {
				r.store(e.Event)
			}
		case *nostr.CloseEnvelope:
			// The relay AMQ Remote runs against answers CLOSE with CLOSED.
			closed, _ := nostr.ClosedEnvelope{SubscriptionID: string(*e), Reason: "closed"}.MarshalJSON()
			send(closed)
		case *nostr.ReqEnvelope:
			if r.BeforeReq != nil {
				r.BeforeReq(e.Filters)
			}
			sub := &liveSub{id: e.SubscriptionID, filters: e.Filters, send: send}
			r.mu.Lock()
			stored := append([]nostr.Event(nil), r.stored...)
			if r.live == nil {
				r.live = map[*liveSub]struct{}{}
			}
			r.live[sub] = struct{}{}
			r.mu.Unlock()
			for _, evt := range stored {
				for _, f := range e.Filters {
					if f.Matches(evt) {
						id := e.SubscriptionID
						frame, _ := nostr.EventEnvelope{SubscriptionID: &id, Event: evt}.MarshalJSON()
						send(frame)
						break
					}
				}
			}
			eose, _ := nostr.EOSEEnvelope{SubscriptionID: e.SubscriptionID}.MarshalJSON()
			send(eose)
			defer func() {
				r.mu.Lock()
				delete(r.live, sub)
				r.mu.Unlock()
			}()
		}
	}
}

func (r *Relay) checkAuth(evt nostr.Event, challenge, relayURL string) string {
	if evt.Kind != 22242 || !evt.VerifySignature() || evt.PubKey.Hex() != r.body {
		return "invalid: auth event kind, signature or pubkey"
	}
	var gotRelay, gotChallenge bool
	var auth [][]string
	for _, tag := range evt.Tags {
		switch {
		case len(tag) == 2 && tag[0] == "relay":
			gotRelay = tag[1] == relayURL
		case len(tag) == 2 && tag[0] == "challenge":
			gotChallenge = tag[1] == challenge
		case len(tag) > 0 && tag[0] == "auth":
			auth = append(auth, tag)
		}
	}
	if !gotRelay || !gotChallenge {
		return "invalid: relay or challenge tag"
	}
	if len(auth) != 1 {
		return fmt.Sprintf("invalid: want exactly one auth tag, got %d", len(auth))
	}
	t, err := bodykey.ParseAuthTag(auth[0])
	if err != nil || t.Verify(r.body) != nil || strings.Join(auth[0], "|") != strings.Join(r.tag, "|") {
		return "restricted: NIP-OA tag does not verify for this body"
	}
	return ""
}
