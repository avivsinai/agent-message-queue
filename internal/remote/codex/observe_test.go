package codex

import (
	"encoding/json"
	"testing"
)

// 611.17: an activity observer sees the notifications the attachment
// handles, after the adapter's own handling, until it is removed.
func TestObserveNotificationsSeesHandledNotifications(t *testing.T) {
	a := &Attachment{threadID: "t1", targetID: "codex:t1"}
	var seen []string
	stop := a.ObserveNotifications(func(n Notification) { seen = append(seen, n.Method) })
	started := Notification{Method: "turn/started", Params: json.RawMessage(`{"threadId":"t1","turn":{"id":"u1"}}`)}
	a.onNotification(started)
	if a.activeTurn != "u1" || len(seen) != 1 || seen[0] != "turn/started" {
		t.Fatalf("activeTurn=%q seen=%v, want the adapter to handle it and the observer to see it", a.activeTurn, seen)
	}
	stop()
	a.onNotification(started)
	if len(seen) != 1 {
		t.Fatalf("seen=%v after stop, want no more", seen)
	}
}
