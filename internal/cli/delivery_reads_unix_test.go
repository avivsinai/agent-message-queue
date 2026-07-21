//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestCapabilityConfigReadRefusesRetargetedAlias(t *testing.T) {
	parent := secureTempDirForTest(t)
	alias := filepath.Join(parent, "authorized")
	parked := filepath.Join(parent, "authorized-parked")
	replacement := filepath.Join(parent, "replacement")
	for _, root := range []string{alias, replacement} {
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(alias, "meta", "config.json"), []byte(`{"agents":["alice","bob"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "meta", "config.json"), []byte(`{"agents":["alice","mallory"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(alias, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Rename(alias, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}

	err = validateKnownHandlesDeliveryRoot(root, true, "mallory")
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("strict config validation error = %v, want retarget refusal", err)
	}
}

func TestCapabilityMessageReadRefusesRetargetedAlias(t *testing.T) {
	parent := secureTempDirForTest(t)
	alias := filepath.Join(parent, "authorized")
	parked := filepath.Join(parent, "authorized-parked")
	replacement := filepath.Join(parent, "replacement")
	const agent = "alice"
	const filename = "msg.md"
	for _, root := range []string{alias, replacement} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	writeMessage := func(root, from, body string) {
		t.Helper()
		msg := format.Message{Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      "msg",
			From:    from,
			To:      []string{agent},
			Thread:  "p2p/alice__bob",
			Created: "2026-07-21T00:00:00Z",
		}, Body: body}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, agent), filename), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMessage(alias, "bob", "authorized")
	writeMessage(replacement, "mallory", "forged")
	identity, err := fsq.SnapshotDeliveryRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(alias, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Rename(alias, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}

	_, _, err = findMessageDeliveryRoot(root, agent, filename, false)
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("message discovery error = %v, want retarget refusal", err)
	}
}
