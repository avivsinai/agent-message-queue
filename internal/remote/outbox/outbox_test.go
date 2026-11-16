package outbox

import (
	"bytes"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

func TestRecordKeepsItsEventAcrossAttemptAndReopen(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1794689428, 0)
	sk := nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002")
	signed := func(content string) nostr.Event {
		evt := nostr.Event{Kind: 9, Tags: nostr.Tags{{"h", "e4788682-3227-4e30-9e96-34684b749593"}}, Content: "<b>" + content + "</b>", CreatedAt: nostr.Timestamp(now.Unix())}
		if err := evt.Sign(sk); err != nil {
			t.Fatal(err)
		}
		return evt
	}
	open := func() *Outbox {
		t.Helper()
		o, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	get := func() Record {
		t.Helper()
		r, ok, err := open().Get("send/reg-1/welcome")
		if err != nil || !ok {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		return r
	}

	first := signed("first")
	o := open()
	prepared, err := o.Prepare("send/reg-1/welcome", "reg-1", first, now)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := first.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Outcome != buzzop.NotSent || prepared.EventID != first.ID.Hex() || prepared.Seq != 1 || !bytes.Equal(prepared.Event, exact) {
		t.Fatalf("prepared = %+v", prepared)
	}
	if left, err := o.Unsettled(); err != nil || len(left) != 1 {
		t.Fatalf("unsettled after prepare = %v, %v", left, err)
	}
	if _, err := o.BeginAttempt("send/reg-1/welcome", now); err != nil {
		t.Fatal(err)
	}

	r := get()
	if r.Outcome != buzzop.Unknown || r.Attempts != 1 || !bytes.Equal(r.Event, prepared.Event) {
		t.Fatalf("after attempt = %+v", r)
	}
	if _, err := open().Settle("send/reg-1/welcome", OutcomeOf(nil), "", now, now); err != nil {
		t.Fatal(err)
	}
	r = get()
	if r.Outcome != buzzop.Accepted || !bytes.Equal(r.Event, prepared.Event) {
		t.Fatalf("after settle = %+v", r)
	}
	if left, err := open().Unsettled(); err != nil || len(left) != 0 {
		t.Fatalf("unsettled after accept = %v, %v", left, err)
	}
	again, err := open().Prepare("send/reg-1/welcome", "reg-1", signed("second"), now)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := again.SignedEvent()
	if err != nil || stored.ID != first.ID || !stored.VerifySignature() {
		t.Fatalf("second prepare returned event %v (err %v), want the first %s", stored.ID.Hex(), err, first.ID.Hex())
	}
}

// The outbox forgets old accepted and rejected records, whose intentions
// no writer asks again after its keep. Unknown stays forever: only its
// own event may be sent again without a risk of a second delivery.
func TestPruneForgetsOldSettledRecordsAndKeepsUnknown(t *testing.T) {
	dir := t.TempDir()
	o, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1794689428, 0)
	sk := nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000002")
	evt := nostr.Event{Kind: 9, Tags: nostr.Tags{{"h", "e4788682-3227-4e30-9e96-34684b749593"}}, CreatedAt: nostr.Timestamp(t0.Unix())}
	if err := evt.Sign(sk); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		key     string
		outcome buzzop.Outcome
		at      time.Time
	}{
		{"a", buzzop.Accepted, t0}, {"b", buzzop.Rejected, t0},
		{"c", buzzop.Unknown, t0}, {"d", buzzop.Accepted, t0.Add(80 * time.Hour)},
	} {
		if _, err := o.Prepare(r.key, "reg", evt, r.at); err != nil {
			t.Fatal(err)
		}
		if _, err := o.BeginAttempt(r.key, r.at); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Settle(r.key, r.outcome, "", r.at, r.at); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := o.Prune(t0.Add(81*time.Hour), 72*time.Hour); err != nil || removed != 2 {
		t.Fatalf("Prune = %d, %v; want 2 removed", removed, err)
	}
	for _, key := range []string{"c", "d"} {
		if _, ok, err := o.Get(key); err != nil || !ok {
			t.Fatalf("%q must stay: %v, %v", key, ok, err)
		}
	}
	for _, key := range []string{"a", "b"} {
		if _, ok, err := o.Get(key); err != nil || ok {
			t.Fatalf("%q settled long ago must be forgotten: %v, %v", key, ok, err)
		}
	}
	stats, err := Stat(dir)
	if err != nil || stats.Records != 2 || stats.OldestUnknown != t0.Unix() {
		t.Fatalf("Stat = %+v, %v; want two records and unknown from t0", stats, err)
	}
}
