package amqio

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for agent-message-queue-611.22.44: the reply ledger had two
// filename-validation holes.
//
//  1. readLedger ran the id through fsq.ValidateMessageFilename(msgID+".md")
//     — a filename validator that requires a .md suffix — purely as a way to
//     check id safety, NOT to select the read path (the file read is
//     msgID+".json"). On ANY validation failure it returned (nil, nil).
//     Callers treat (nil, nil) as "no ledger exists," so a malformed id
//     silently dropped a ledger that should have been re-delivered, and a
//     fresh one was written over the (now-invisible) existing reply. The
//     real defect was suppressing the validation error, not the synthetic
//     suffix.
//
//  2. writeLedger had NO filename validation at all. The ledger file is built
//     as <msgID>.json under ledgerDir, so an id carrying a path separator or
//     ".." would write outside ledgerDir — a path-traversal write.
//
// The fix introduces validateLedgerID (base-name-only, no separators/traversal/
// NUL/dotfile/absolute) used by BOTH readLedger (returns an error, not
// nil,nil) and writeLedger (rejects before touching the filesystem).

// Every invalid id is refused by writeLedger before it touches the
// filesystem, and by readLedger with an error rather than (nil, nil).
func TestB44LedgerRejectsInvalidIDs(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(endpointRoot, "agents", DefaultHandle, "extensions", "remote", "replies"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &Carrier{root: endpointRoot, me: DefaultHandle}
	for _, evil := range []string{"../../evil", "a/b", "a\\b", ".hidden", "..", "/abs/path", "x\x00y"} {
		if err := c.writeLedger(evil, &replyRecord{ID: "x", To: "codex", Data: []byte("{}")}); err == nil {
			t.Fatalf("writeLedger accepted invalid id %q (611.22.44 — must reject before touching the filesystem)", evil)
		}
		rec, err := c.readLedger(evil)
		if err == nil || rec != nil {
			t.Fatalf("readLedger(%q) = (%v, %v), want an error (611.22.44 — must surface the error, not silently drop as (nil,nil))", evil, rec, err)
		}
	}
	// ../../evil from <root>/agents/<me>/extensions/remote/replies would land
	// at <root>/agents/<me>/extensions/evil.json.
	escaped := filepath.Join(endpointRoot, "agents", DefaultHandle, "extensions", "evil.json")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("path-traversal write escaped the ledger dir to %s (611.22.44)", escaped)
	}
}
