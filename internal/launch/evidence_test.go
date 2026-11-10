package launch

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestImmutableCaptureEvidenceReadback(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "70707070-7070-4070-8070-707070707070"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC)
	capture, err := ParseCodexNotifyEvidence(codexNotifyTestPayload("019c8a2f-2b13-7000-8000-000000000001", fixture.cwd), nonce, "claude", codexCaptureMinimumVersion, fixture.cwd)
	if err != nil {
		t.Fatal(err)
	}
	request := EvidenceWriteRequest{
		Kind: EvidenceProviderCapture, Handle: "claude", ObservedAt: observed,
		Payload: capture.payload, CallerContext: map[string]string{"run_id": "run-42"},
	}
	providerRef, err := WriteEvidence(fixture.root, lease, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEvidence(fixture.root, lease, request); err == nil {
		t.Fatal("same content-addressed evidence overwrote its existing record")
	} else {
		var exists *EvidenceExistsError
		if !errors.As(err, &exists) || exists.ID != providerRef.ID {
			t.Fatalf("duplicate evidence error = %v", err)
		}
	}
	record, readRef, err := ReadEvidence(fixture.root, providerRef.ID)
	if err != nil || !reflect.DeepEqual(record.CallerContext, request.CallerContext) || !reflect.DeepEqual(readRef.CallerContext, request.CallerContext) {
		t.Fatalf("evidence caller context readback = record %#v ref %#v err %v", record.CallerContext, readRef.CallerContext, err)
	}
	changedContext := request
	changedContext.CallerContext = map[string]string{"run_id": "run-43"}
	changedRef, err := WriteEvidence(fixture.root, lease, changedContext)
	if err != nil {
		t.Fatal(err)
	}
	if changedRef.ID == providerRef.ID {
		t.Fatal("caller context did not enter immutable evidence digest")
	}
	manualRef, err := WriteEvidence(fixture.root, lease, EvidenceWriteRequest{
		Kind: EvidenceManual, Handle: "claude", ObservedAt: observed.Add(time.Second), Payload: []byte(`{"claimed":"provider"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	manualConversation := ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: CodexProvider, ID: "019c8a2f-2b13-7000-8000-000000000001"}, ProviderVersion: codexCaptureMinimumVersion, LaunchNonce: nonce,
		ExecutionEvidence: &ConversationExecutionEvidence{
			Backend: LauncherTMux, Profile: TmuxProfile().Identity(), Outcome: OutcomeCreated,
			LaunchNonce: nonce, ConversationID: "019c8a2f-2b13-7000-8000-000000000001",
		},
		EvidenceRefs: []string{manualRef.ID},
	}
	if err := WriteConversation(fixture.root, lease, manualConversation); err == nil || !strings.Contains(err.Error(), "requires provider_capture") {
		t.Fatalf("manual evidence inflated continuity: %v", err)
	}
	manualPath := EvidencePath(fixture.session, manualRef.ID)
	manualData, err := os.ReadFile(manualPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		old  []byte
		new  []byte
	}{
		{name: "kind inflation", old: []byte(`"kind":"manual"`), new: []byte(`"kind":"provider_capture"`)},
		{name: "handle substitution", old: []byte(`"handle":"claude"`), new: []byte(`"handle":"peerxx"`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := bytes.Replace(manualData, test.old, test.new, 1)
			if bytes.Equal(mutated, manualData) {
				t.Fatal("tamper target was not found")
			}
			if err := os.WriteFile(manualPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(manualPath, manualData, 0o600); err != nil {
					t.Errorf("restore evidence: %v", err)
				}
			})
			_, _, readErr := ReadEvidence(fixture.root, manualRef.ID)
			var corrupt *EvidenceCorruptError
			if !errors.As(readErr, &corrupt) {
				t.Fatalf("non-payload tamper read error = %v", readErr)
			}
			if err := WriteConversation(fixture.root, lease, manualConversation); !errors.As(err, &corrupt) {
				t.Fatalf("non-payload tamper gated continuity: %v", err)
			}
		})
	}
	manualConversation.EvidenceRefs = []string{providerRef.ID, manualRef.ID}
	if err := WriteConversation(fixture.root, lease, manualConversation); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	identity, err := fsq.SnapshotDeliveryRoot(fixture.session)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := fsq.OpenDeliveryRoot(fixture.session, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	record, gotRef, err := ReadEvidence(restarted, providerRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRef, providerRef) || record.Kind != EvidenceProviderCapture || record.PayloadSHA256 == "" {
		t.Fatalf("restart evidence = %#v %#v", record, gotRef)
	}
	conversation, err := LoadConversation(restarted, "claude")
	if err != nil || !reflect.DeepEqual(conversation.EvidenceRefs, []string{providerRef.ID, manualRef.ID}) {
		t.Fatalf("restart conversation evidence = %#v, %v", conversation.EvidenceRefs, err)
	}

	path := EvidencePath(fixture.session, providerRef.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ReadEvidence(restarted, providerRef.ID)
	var corrupt *EvidenceCorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("tampered evidence error = %v", err)
	}
	if _, err := LoadConversation(restarted, "claude"); !errors.As(err, &corrupt) {
		t.Fatalf("tampered continuity readback error = %v", err)
	}
}

func persistCursorEvidenceForTest(t *testing.T, fixture executionFixture, lease *Lease, handle, nonce, id string) string {
	t.Helper()
	capture, err := ParseCursorCreateChatEvidence([]byte(id+"\n"), nonce, handle, cursorCaptureVersion)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := persistProviderCaptureEvidenceWithContext(fixture.root, lease, handle, nil, []CaptureEvidence{capture})
	if err != nil || len(refs) != 1 {
		t.Fatalf("persist refs=%v err=%v", refs, err)
	}
	return refs[0]
}

// One handle's unreadable evidence must not fail the capture lookup of another
// handle (audit 2026-11-10).
func TestProviderCaptureLookupReadsOnlyTheHandlesEvidence(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "71717171-7171-4171-8171-717171717171"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor", "other"); err != nil {
		t.Fatal(err)
	}
	refA := persistCursorEvidenceForTest(t, fixture, lease, "cursor", nonce, "019c8a2f-2b13-7000-8000-00000000000a")
	refB := persistCursorEvidenceForTest(t, fixture, lease, "other", nonce, "019c8a2f-2b13-7000-8000-00000000000b")
	if err := os.WriteFile(EvidencePath(fixture.session, refB), []byte("not evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, id, found, err := findProviderCaptureEvidence(fixture.root, CursorProvider, "cursor", nonce, cursorCaptureVersion)
	if err != nil || !found || id != refA {
		t.Fatalf("lookup for cursor = %q found=%v err=%v, want %q", id, found, err, refA)
	}
	if _, _, found, err := findProviderCaptureEvidence(fixture.root, CursorProvider, "other", nonce, cursorCaptureVersion); err == nil || found {
		t.Fatalf("lookup for the corrupted handle found=%v err=%v, want its own error", found, err)
	}
	if _, _, found, err := findProviderCaptureEvidence(fixture.root, CursorProvider, "claude", nonce, cursorCaptureVersion); err != nil || found {
		t.Fatalf("lookup for a handle with no evidence found=%v err=%v", found, err)
	}
}

// A session written before the per-handle index still finds its evidence.
func TestProviderCaptureLookupScansAnIndexlessSession(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "72727272-7272-4272-8272-727272727272"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor"); err != nil {
		t.Fatal(err)
	}
	ref := persistCursorEvidenceForTest(t, fixture, lease, "cursor", nonce, "019c8a2f-2b13-7000-8000-00000000000c")
	if err := os.RemoveAll(filepath.Join(fixture.session, evidenceHandleIndex)); err != nil {
		t.Fatal(err)
	}
	_, id, found, err := findProviderCaptureEvidence(fixture.root, CursorProvider, "cursor", nonce, cursorCaptureVersion)
	if err != nil || !found || id != ref {
		t.Fatalf("old-layout lookup = %q found=%v err=%v, want %q", id, found, err, ref)
	}
}

// Evidence an older build wrote stays findable after a new write creates the
// handle index (audit 2026-11-10).
func TestProviderCaptureLookupSurvivesFirstIndexedWriteInOldSession(t *testing.T) {
	fixture := newExecutionFixture(t)
	nonce := "73737373-7373-4373-8373-737373737373"
	lease, err := AcquireLease(fixture.root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("cursor", "other"); err != nil {
		t.Fatal(err)
	}
	refY := persistCursorEvidenceForTest(t, fixture, lease, "cursor", nonce, "019c8a2f-2b13-7000-8000-00000000000d")
	// Make the session look like an older build wrote it: no index at all.
	if err := os.RemoveAll(filepath.Join(fixture.session, evidenceHandleIndex)); err != nil {
		t.Fatal(err)
	}
	persistCursorEvidenceForTest(t, fixture, lease, "other", nonce, "019c8a2f-2b13-7000-8000-00000000000e")
	_, id, found, err := findProviderCaptureEvidence(fixture.root, CursorProvider, "cursor", nonce, cursorCaptureVersion)
	if err != nil || !found || id != refY {
		t.Fatalf("lookup after first indexed write = %q found=%v err=%v, want %q", id, found, err, refY)
	}
}
