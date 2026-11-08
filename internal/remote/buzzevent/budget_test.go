package buzzevent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/avivsinai/agent-message-queue/internal/relay"
)

// Review of the event fetch: a relay that accepts the connection and then
// says nothing held Fetch for about 7 s, because Connect's failure path
// waited for a close handshake the relay never answered.
func TestFetchReturnsWithinItsBudgetWhenTheRelaySendsNoAuthChallenge(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-release // never answers, never reads
		_ = c.CloseNow()
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	var sk [32]byte
	sk[31] = 1
	cfg := relay.Config{
		URL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		Secret:  sk,
		AuthTag: []string{"auth", strings.Repeat("a", 64), "", strings.Repeat("b", 128)},
	}
	start := time.Now()
	_, err := Fetch(context.Background(), cfg, strings.Repeat("c", 64), "x")
	if d := time.Since(start); err == nil || d > Budget+500*time.Millisecond {
		t.Fatalf("Fetch = %v after %v, want a failure within its %v budget", err, d, Budget)
	}
}
