package buzzio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A claim and a prepared output are created once and replayed
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
	if err := l.MarkAccepted("row/ref-1/1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if pending, _ := l.Pending(); len(pending) != 0 {
		t.Fatalf("pending after accept = %v, want none", pending)
	}
}

// Regression: #971, an earlier build
// stored a refusal as a bare reason string, and one such record failed the
// whole outbox scan. It reads as one refused attempt, due now.
func TestLedgerReadsBareRefusalReason(t *testing.T) {
	stateDir := t.TempDir()
	l, err := OpenLedger(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	old := `{"key":"row/ref-1/00000000","event":{"id":"one"},"binding":{},"accepted":false,"refused":"invalid: too old"}`
	if err := os.WriteFile(filepath.Join(stateDir, "buzz", "outbox", keyFile("row/ref-1/00000000")), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := l.Pending()
	if err != nil || len(pending) != 1 || pending[0].Refused == nil || pending[0].Refused.Reason != "invalid: too old" || pending[0].Refused.Attempts != 1 || !pending[0].Due(time.Now()) {
		t.Fatalf("pending = %+v err=%v, want the old record owed, refused once, due now", pending, err)
	}
}

// agent-message-queue-611.71: one truncated accepted outbox row failed the
// whole outbox scan, so Flush sent no healthy reply, and after a restart
// nextSeq failed on the same row, so every new Prepare failed too.
func TestLedgerIsolatesADamagedOutboxRow(t *testing.T) {
	stateDir := t.TempDir()
	l, err := OpenLedger(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Prepare("row/old/1", json.RawMessage(`{"id":"old"}`), ShareBinding{}); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkAccepted("row/old/1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Prepare("row/new/1", json.RawMessage(`{"id":"new"}`), ShareBinding{}); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(stateDir, "buzz", "outbox", keyFile("row/old/1"))
	if err := os.WriteFile(damaged, []byte(`{"key":"row/old/1","acc`), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := l.Pending(); err != nil || len(pending) != 1 || pending[0].Key != "row/new/1" {
		t.Fatalf("pending = %+v err=%v, want the healthy reply", pending, err)
	}
	restarted, err := OpenLedger(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Prepare("row/new/2", json.RawMessage(`{"id":"new2"}`), ShareBinding{}); err != nil {
		t.Fatalf("prepare after restart: %v", err)
	}
	if rows, err := DamagedOutputs(stateDir); err != nil || len(rows) != 1 || rows[0].Path != damaged {
		t.Fatalf("damaged outputs = %+v err=%v, want the truncated row", rows, err)
	}
}
