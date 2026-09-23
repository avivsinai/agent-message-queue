package buzzio

import (
	"encoding/json"
	"strings"
	"testing"
)

// 611.16: a claim and a prepared output are created once and replayed
// verbatim; an accepted output leaves the pending list.
func TestLedgerReplaysClaimsAndPreparedOutputVerbatim(t *testing.T) {
	l, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ab", 32)
	first, created, err := l.Claim(Claim{EventID: id, Owner: "o", Op: "submit", Target: "cx", Epoch: "e1", RequestID: "r1"})
	if err != nil || !created {
		t.Fatalf("first claim: created=%v err=%v", created, err)
	}
	again, created, err := l.Claim(Claim{EventID: id, Owner: "o", Op: "submit", Target: "other", Epoch: "e2", RequestID: "r2"})
	if err != nil || created || again.Target != first.Target || again.RequestID != first.RequestID {
		t.Fatalf("replayed claim = %+v created=%v err=%v, want the first claim verbatim", again, created, err)
	}

	signed := json.RawMessage(`{"id":"one"}`)
	if _, err := l.Prepare("row/ref-1/1", signed, ShareBinding{}); err != nil {
		t.Fatal(err)
	}
	o, err := l.Prepare("row/ref-1/1", json.RawMessage(`{"id":"re-signed"}`), ShareBinding{})
	if err != nil || string(o.Event) != string(signed) {
		t.Fatalf("re-prepare returned %s err=%v, want the original bytes", o.Event, err)
	}
	pending, err := l.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v err=%v, want one", pending, err)
	}
	if err := l.MarkAccepted("row/ref-1/1"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := l.Pending(); len(pending) != 0 {
		t.Fatalf("pending after accept = %v, want none", pending)
	}
}
