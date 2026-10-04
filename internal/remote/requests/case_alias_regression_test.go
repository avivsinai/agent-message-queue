package requests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestCaseAliasKeysDoNotShareARecord is the agent-message-queue-611.49
// regression (Pro review of #934): on a case-insensitive filesystem a key
// that differs only by case in creator_host or target_id maps to the same
// file. Get returned the other key's record, and Create saw it as its own.
func TestCaseAliasKeysDoNotShareARecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PROBE")); err != nil {
		t.Skip("filesystem is case-sensitive; keys cannot alias")
	}
	s, err := Open(dir, WithClock(fixedClock))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	orig := newRecord("11111111-1111-4111-8111-111111111149")
	orig.CreatorHost, orig.TargetID = "buzz-ab12", "t_fake1"
	if err := s.Create(orig); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, alias := range []Key{
		{CreatorHost: "BUZZ-AB12", TargetID: orig.TargetID, RequestID: orig.RequestID},
		{CreatorHost: orig.CreatorHost, TargetID: "T_FAKE1", RequestID: orig.RequestID},
	} {
		if rec, exists, _ := s.Get(alias); exists || rec != nil {
			t.Fatalf("Get(%+v) returned the record of %s", alias, orig.RequestRef)
		}
		other := newRecord(alias.RequestID)
		other.CreatorHost, other.TargetID = alias.CreatorHost, alias.TargetID
		other.InputDigest = Digest([]byte("other"))
		if err := s.Create(other); protocol.RefusalCode(err) != protocol.CodeRequestConflict {
			t.Fatalf("Create(%+v) = %v, want request_conflict", alias, err)
		}
	}

	got, exists, err := s.Get(keyOf(orig))
	if err != nil || !exists || got.CreatorHost != orig.CreatorHost || got.TargetID != orig.TargetID || got.InputDigest != orig.InputDigest {
		t.Fatalf("original record changed: %+v exists=%v err=%v", got, exists, err)
	}
}
