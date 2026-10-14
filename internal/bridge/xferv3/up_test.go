package xferv3

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestUpRefusesEmptyLedgerBesideMail(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := CheckEmptyLedger(root, false); err != nil {
		t.Fatalf("empty mail: %v", err)
	}
	inbox := filepath.Join(root, "agents", "codex", "inbox", "new", "hello.md")
	if err := os.WriteFile(inbox, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckEmptyLedger(root, false); err != ErrEmptyLedger {
		t.Fatalf("empty ledger = %v, want ErrEmptyLedger", err)
	}
	if err := CheckEmptyLedger(root, true); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if err := CheckEmptyLedger(root, false); err != nil {
		t.Fatalf("after acknowledge: %v", err)
	}
}

func TestDiscardRemovesOwedAndRecordsFact(t *testing.T) {
	mac := newPeerHost(t, "mac", "codex", bytes.Repeat([]byte{0x51}, 32))
	box := newPeerHost(t, "box", "grok", bytes.Repeat([]byte{0x52}, 32))
	pinPeers(t, &mac, &box)
	idText := writeOwed(t, mac, box, "discard-me", "drop this")
	id, err := ParseID(idText)
	if err != nil {
		t.Fatal(err)
	}
	if err := Discard(mac.root, box.host, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mac.root, "bridge", "tx", "box", idText)); !os.IsNotExist(err) {
		t.Fatalf("owed file remains: %v", err)
	}
	facts, torn, err := readLedger(ledgerPath(mac.root, id))
	if err != nil || torn {
		t.Fatalf("ledger: torn=%v err=%v", torn, err)
	}
	if foldState(facts) != StateDiscard {
		t.Fatalf("state = %q", foldState(facts))
	}
}

func TestStatusListsOwedAndHoldPIDIsSingleton(t *testing.T) {
	mac := newPeerHost(t, "mac", "codex", bytes.Repeat([]byte{0x53}, 32))
	box := newPeerHost(t, "box", "grok", bytes.Repeat([]byte{0x54}, 32))
	pinPeers(t, &mac, &box)
	idText := writeOwed(t, mac, box, "owed-status", "still here")
	report, err := CollectStatus(mac.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Owed) != 1 || report.Owed[0].ID != idText || report.Owed[0].Peer != "box" {
		t.Fatalf("status = %+v", report)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- HoldPID(mac.root, func() error {
			close(started)
			time.Sleep(200 * time.Millisecond)
			return nil
		})
	}()
	<-started
	if err := HoldPID(mac.root, func() error { return nil }); err != ErrUpRunning {
		t.Fatalf("second up = %v, want ErrUpRunning", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLaunchdPlistNamesBridgeUp(t *testing.T) {
	text := LaunchdPlist("/usr/local/bin/amq-bridge", "/var/amq")
	if !strings.Contains(text, "io.github.avivsinai.amq-bridge-up") || !strings.Contains(text, "--root") {
		t.Fatalf("plist = %s", text)
	}
}
