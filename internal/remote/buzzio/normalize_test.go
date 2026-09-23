package buzzio

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func ownerEvent(t *testing.T, secret [32]byte, channel, content string, at time.Time) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Timestamp(at.Unix()), Kind: 9, Content: content, Tags: nostr.Tags{{"h", channel}}}
	if err := evt.Sign(secret); err != nil {
		t.Fatal(err)
	}
	return evt
}

// 611.16: an owner DM becomes a submit with a deterministic request id and a
// two-minute window from its signed time; slash commands map to their ops;
// a non-owner or other-channel event is ignored; a stale mutation never
// executes.
func TestNormalizeOwnerCommands(t *testing.T) {
	var owner, other [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(other[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: "body", Channel: "dm-1", Target: "cx", RelayHost: "relay"}
	now := time.Now()

	submit := ownerEvent(t, owner, "dm-1", "fix the build", now)
	n, err := Normalize(submit, b, now)
	if err != nil || n.Op != OpSubmit || n.Text != "fix the build" || !n.NotAfter.Equal(time.Unix(int64(submit.CreatedAt), 0).Add(MutationWindow)) {
		t.Fatalf("submit = %+v err=%v", n, err)
	}
	if again, _ := Normalize(submit, b, now.Add(time.Second)); again.RequestID != n.RequestID || n.RequestID == "" {
		t.Fatalf("request id not deterministic: %q vs %q", n.RequestID, again.RequestID)
	}
	for text, op := range map[string]string{"/inspect": OpInspect, "/status amqr1_x": OpStatus, "/cancel amqr1_x": OpCancel, "/frobnicate": OpUnsupported} {
		if got, err := Normalize(ownerEvent(t, owner, "dm-1", text, now), b, now); err != nil || got.Op != op {
			t.Fatalf("%q -> %+v err=%v, want op %s", text, got, err, op)
		}
	}
	if _, err := Normalize(ownerEvent(t, other, "dm-1", "hi", now), b, now); !errors.Is(err, ErrNotForUs) {
		t.Fatalf("non-owner: err=%v, want ErrNotForUs", err)
	}
	if _, err := Normalize(ownerEvent(t, owner, "elsewhere", "hi", now), b, now); !errors.Is(err, ErrNotForUs) {
		t.Fatalf("other channel: err=%v, want ErrNotForUs", err)
	}
	if _, err := Normalize(ownerEvent(t, owner, "dm-1", "late", now.Add(-3*time.Minute)), b, now); !errors.Is(err, ErrStale) {
		t.Fatalf("stale submit: err=%v, want ErrStale", err)
	}
}
