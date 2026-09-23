// Package relaytest is an in-process Nostr relay for AMQ relay tests.
package relaytest

import (
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

	mu     sync.Mutex
	stored []nostr.Event
	live   map[*liveSub]struct{}
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
			reason := ""
			if !authed {
				reason = "auth-required: authenticate first"
			} else if !e.VerifySignature() {
				reason = "invalid: bad signature"
			}
			ok, _ := nostr.OKEnvelope{EventID: e.Event.ID, OK: reason == "", Reason: reason}.MarshalJSON()
			send(ok)
			if reason == "" {
				r.store(e.Event)
			}
		case *nostr.ReqEnvelope:
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
