// Package relaytest is an in-process Nostr relay for AMQ relay tests.
package relaytest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ctx := req.Context()
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
			_ = ws.Write(ctx, websocket.MessageText, ok)
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
			_ = ws.Write(ctx, websocket.MessageText, ok)
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
