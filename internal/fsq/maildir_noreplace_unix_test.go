//go:build unix

package fsq

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixNoReplacePublicationAndClaimPreservesCollision(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	const filename = "collide.md"
	original := []byte("retained new copy")
	attacker := []byte("colliding attempt")
	newPath := filepath.Join(AgentInboxNew(base, "alice"), filename)
	if err := os.WriteFile(newPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	root := openDeliveryRootForTest(t, base)
	_, err := DeliverToInbox(root, "alice", filename, attacker)
	if err == nil {
		t.Fatal("DeliverToInbox replaced an unread new/ copy")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision error = %T %v, want EEXIST", err, err)
	}
	got, readErr := os.ReadFile(newPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("new/ bytes = %q, %v; collision must not overwrite unread mail", got, readErr)
	}
	tmpEntries := regularTmpFiles(t, AgentInboxTmp(base, "alice"))
	if len(tmpEntries) != 1 {
		t.Fatalf("tmp leftovers = %#v, want the colliding attempt", tmpEntries)
	}
	got, readErr = os.ReadFile(filepath.Join(AgentInboxTmp(base, "alice"), tmpEntries[0]))
	if readErr != nil || !bytes.Equal(got, attacker) {
		t.Fatalf("tmp bytes = %q, %v; colliding attempt must survive", got, readErr)
	}

	_, err = DeliverToInbox(root, "alice", filename, original)
	if err != nil {
		t.Fatalf("identical-byte publish = %v, want idempotent success", err)
	}
	got, readErr = os.ReadFile(newPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("idempotent new/ bytes = %q, %v", got, readErr)
	}
	if leftovers := regularTmpFiles(t, AgentInboxTmp(base, "alice")); len(leftovers) != 1 {
		t.Fatalf("idempotent retry left tmp = %#v, want only the earlier colliding attempt", leftovers)
	}

	curPath := filepath.Join(AgentInboxCur(base, "alice"), filename)
	if err := os.WriteFile(curPath, []byte("retained cur copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = MoveNewToCur(root, "alice", filename)
	var collision *ClaimCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("claim error = %T %v, want *ClaimCollisionError", err, err)
	}
	got, readErr = os.ReadFile(curPath)
	if readErr != nil || string(got) != "retained cur copy" {
		t.Fatalf("cur bytes = %q, %v; claim must not replace retained cur/", got, readErr)
	}
	got, readErr = os.ReadFile(newPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("new bytes = %q, %v; claim collision must not consume new/", got, readErr)
	}
}

func regularTmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}
