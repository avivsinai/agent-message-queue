package amqio

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for agent-message-queue-611.22.44: the reply ledger had two
// filename-validation holes.
//
//  1. readLedger validated msgID+".md" (the wrong suffix — the file read is
//     msgID+".json") and, on ANY validation failure, returned (nil, nil).
//     Callers treat (nil, nil) as "no ledger exists," so a malformed id
//     silently dropped a ledger that should have been re-delivered, and a
//     fresh one was written over the (now-invisible) existing reply.
//
//  2. writeLedger had NO filename validation at all. The ledger file is built
//     as <msgID>.json under ledgerDir, so an id carrying a path separator or
//     ".." would write outside ledgerDir — a path-traversal write.
//
// The fix introduces validateLedgerID (base-name-only, no separators/traversal/
// NUL/dotfile/absolute) used by BOTH readLedger (returns an error, not
// nil,nil) and writeLedger (rejects before touching the filesystem).

// writeLedger must refuse a path-traversal id: nothing may be written outside
// the ledger directory.
func TestB44WriteLedgerRejectsPathTraversal(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(endpointRoot, "agents", DefaultHandle, "extensions", "remote", "replies"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &Carrier{root: endpointRoot, me: DefaultHandle}

	// A traversal id that, without validation, writes ../../evil.json outside
	// the ledger dir (and outside the agent dir entirely).
	evil := "../../evil"
	err := c.writeLedger(evil, &replyRecord{ID: "x", To: "codex", Data: []byte("{}")})
	if err == nil {
		t.Fatal("writeLedger accepted a path-traversal id (611.22.44 — must reject before touching the filesystem)")
	}
	// Nothing may have been written outside the ledger dir.
	if _, err := os.Stat(filepath.Join(endpointRoot, "evil.json")); err == nil {
		t.Fatal("path-traversal write escaped the ledger dir (611.22.44 — ../../evil.json landed at root)")
	}
}

// writeLedger must refuse a separator-bearing id and a dotfile id.
func TestB44WriteLedgerRejectsSeparatorsAndDotfile(t *testing.T) {
	endpointRoot := t.TempDir()
	c := &Carrier{root: endpointRoot, me: DefaultHandle}
	for _, evil := range []string{"a/b", "a\\b", ".hidden", "..", "/abs/path", "x\x00y"} {
		if err := c.writeLedger(evil, &replyRecord{ID: "x", To: "codex", Data: []byte("{}")}); err == nil {
			t.Fatalf("writeLedger accepted invalid id %q (611.22.44)", evil)
		}
	}
}

// readLedger must NOT return (nil, nil) for an invalid id — that silently
// dropped a ledger that should have been re-delivered. It must return an
// error so the caller can decide.
func TestB44ReadLedgerReturnsErrorNotSilentDropForInvalidID(t *testing.T) {
	endpointRoot := t.TempDir()
	c := &Carrier{root: endpointRoot, me: DefaultHandle}
	for _, evil := range []string{"a/b", "..", "/abs", ".hidden", "x\x00y"} {
		rec, err := c.readLedger(evil)
		if err == nil {
			t.Fatalf("readLedger returned no error for invalid id %q (611.22.44 — must surface the error, not silently drop as (nil,nil))", evil)
		}
		if rec != nil {
			t.Fatalf("readLedger returned a record for invalid id %q (611.22.44)", evil)
		}
	}
}

// A well-formed id still round-trips: readLedger returns (nil, nil) for a
// missing ledger (not an error), and writeLedger then readLedger returns the
// record. This guards against over-broad validation breaking the happy path.
func TestB44LedgerRoundTripsForValidID(t *testing.T) {
	endpointRoot := t.TempDir()
	c := &Carrier{root: endpointRoot, me: DefaultHandle}
	const id = "2026-09-16T06-20-00.000Z_pid1_abcd"

	// Missing ledger → (nil, nil), no error.
	rec, err := c.readLedger(id)
	if err != nil {
		t.Fatalf("readLedger missing valid id errored: %v (611.22.44 — happy path must stay (nil,nil))", err)
	}
	if rec != nil {
		t.Fatalf("readLedger missing valid id returned a record: %+v", rec)
	}
	// Write then read → the record round-trips.
	want := &replyRecord{ID: "reply-id", To: "codex", Data: []byte(`{"ok":true}`)}
	if err := c.writeLedger(id, want); err != nil {
		t.Fatalf("writeLedger valid id errored: %v (611.22.44)", err)
	}
	got, err := c.readLedger(id)
	if err != nil {
		t.Fatalf("readLedger after write errored: %v (611.22.44)", err)
	}
	if got == nil || got.ID != want.ID || string(got.Data) != string(want.Data) {
		t.Fatalf("ledger round-trip mismatch: got %+v, want %+v (611.22.44)", got, want)
	}
	// And the file landed INSIDE the ledger dir, not outside.
	ledgerFile := filepath.Join(endpointRoot, "agents", DefaultHandle, "extensions", "remote", "replies", id+".json")
	if _, err := os.Stat(ledgerFile); err != nil {
		t.Fatalf("ledger file not at expected path %s: %v (611.22.44)", ledgerFile, err)
	}
}
