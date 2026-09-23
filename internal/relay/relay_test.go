package relay_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

func testIdentity(t *testing.T) (*bodykey.BodyKey, []string) {
	t.Helper()
	body, err := bodykey.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	tag, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(1059, time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	return body, []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
}

// 611.15 slice 1: a configured body completes challenge -> signed AUTH with
// one NIP-OA tag -> matching OK, publishes with a matching OK, and after the
// relay drops it reconnects and authenticates again under the same identity.
func TestClientAuthenticatesPublishesAndReconnects(t *testing.T) {
	body, tag := testIdentity(t)
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), tag)
	lr.DropOnce.Store(true)
	defer srv.Close()

	c := relay.NewClient(func() (relay.Config, error) {
		return relay.Config{URL: url, Secret: body.Secret(), AuthTag: tag}, nil
	})
	relay.SetBackoffForTest(c, 10*time.Millisecond, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for lr.AuthOK.Load() < 2 || c.Status().State != relay.StateAuthenticated {
		if time.Now().After(deadline) {
			t.Fatalf("auths=%d status=%+v, want a second authentication after the drop", lr.AuthOK.Load(), c.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: 1059, Content: "x"}
	if err := evt.Sign(body.Secret()); err != nil {
		t.Fatal(err)
	}
	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	defer pcancel()
	if err := c.Conn().Publish(pctx, evt); err != nil {
		t.Fatalf("publish after reconnect: %v", err)
	}
}

// codex slice 1 review r2 #3: a grant that expired just after the minute
// check left the connection usable for up to another minute. The
// connection now closes at the signed not-after and refuses to publish.
func TestConnClosesAtGrantExpiry(t *testing.T) {
	body, tag := testIdentity(t)
	_, srv, url := relaytest.Start(body.PublicKeyHex(), tag)
	defer srv.Close()
	notAfter := time.Unix(time.Now().Unix()+2, 0)
	conn, err := relay.Connect(context.Background(), relay.Config{URL: url, Secret: body.Secret(), AuthTag: tag, NotAfter: notAfter})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("connection still open after the grant's not-after")
	}
	if !errors.Is(conn.Err(), relay.ErrGrantExpired) {
		t.Fatalf("conn.Err() = %v, want ErrGrantExpired", conn.Err())
	}
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: 1059, Content: "x"}
	if err := evt.Sign(body.Secret()); err != nil {
		t.Fatal(err)
	}
	if err := conn.Publish(context.Background(), evt); !errors.Is(err, relay.ErrGrantExpired) {
		t.Fatalf("publish after expiry: err=%v, want ErrGrantExpired", err)
	}
}

// codex slice 1 review r3 #2: at expiry the graceful close waited for a
// peer that withheld its close reply, so the expired connection stayed open
// for up to 10 s. Expiry now aborts the transport.
func TestConnExpiryDoesNotWaitForPeerClose(t *testing.T) {
	body, tag := testIdentity(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		challenge := "expiry"
		frame, _ := (nostr.AuthEnvelope{Challenge: &challenge}).MarshalJSON()
		if ws.Write(r.Context(), websocket.MessageText, frame) != nil {
			return
		}
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		env, err := nostr.ParseMessage(string(data))
		if err != nil {
			return
		}
		auth, ok := env.(*nostr.AuthEnvelope)
		if !ok {
			return
		}
		frame, _ = (nostr.OKEnvelope{EventID: auth.Event.ID, OK: true}).MarshalJSON()
		if ws.Write(r.Context(), websocket.MessageText, frame) != nil {
			return
		}
		<-release // never read or answer the close frame
	}))
	defer srv.Close()
	defer close(release)
	notAfter := time.Unix(time.Now().Unix()+2, 0)
	conn, err := relay.Connect(context.Background(), relay.Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Secret: body.Secret(), AuthTag: tag, NotAfter: notAfter})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.Done():
	case <-time.After(time.Until(notAfter) + 500*time.Millisecond):
		t.Fatal("expired connection stays open while the peer withholds its close reply")
	}
}
