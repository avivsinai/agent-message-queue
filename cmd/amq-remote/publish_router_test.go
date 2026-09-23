package main

import (
	"errors"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Relay design §5: publication goes to the record's own carrier; a local
// record publishes nothing, and a carrier that is not running is an error,
// never a silent success that would advance the published revision.
func TestPublishRouterRoutesByOriginCarrier(t *testing.T) {
	var amq int
	route := publishRouter(map[string]publishFunc{
		"amq": func(protocol.Snapshot, map[string]string) error { amq++; return nil },
	})
	if err := route(protocol.Snapshot{}, nil); err != nil || amq != 0 {
		t.Fatalf("local record: err=%v amq=%d, want no publication", err, amq)
	}
	if err := route(protocol.Snapshot{}, map[string]string{"carrier": "amq"}); err != nil || amq != 1 {
		t.Fatalf("amq record: err=%v amq=%d, want one AMQ publication", err, amq)
	}
	if err := route(protocol.Snapshot{}, map[string]string{"carrier": "buzz"}); !errors.Is(err, errCarrierUnavailable) {
		t.Fatalf("buzz record with no buzz carrier: err=%v, want errCarrierUnavailable", err)
	}
}
