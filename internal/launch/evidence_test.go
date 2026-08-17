package launch

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
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
	request := EvidenceWriteRequest{
		Kind: EvidenceProviderCapture, Handle: "claude", ObservedAt: observed,
		Payload: []byte(`{ "params": {"thread":{"id":"019c8a2f-2b13-7000-8000-000000000001"}}, "method":"thread/started" }`),
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
	manualRef, err := WriteEvidence(fixture.root, lease, EvidenceWriteRequest{
		Kind: EvidenceManual, Handle: "claude", ObservedAt: observed.Add(time.Second), Payload: []byte(`{"claimed":"provider"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	manualConversation := ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: CodexProvider, ID: "019c8a2f-2b13-7000-8000-000000000001"}, LaunchNonce: nonce,
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

func TestEvidenceExclusivePublicationRace(t *testing.T) {
	fixture := newExecutionFixture(t)
	lease, err := AcquireLease(fixture.root, "71717171-7171-4171-8171-717171717171")
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	request := EvidenceWriteRequest{
		Kind: EvidenceFixture, Handle: "claude", ObservedAt: time.Date(2026, 8, 17, 2, 3, 4, 0, time.UTC),
		Payload: []byte(`{"fixture":true}`),
	}
	const writers = 12
	results := make(chan error, writers)
	var group sync.WaitGroup
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := WriteEvidence(fixture.root, lease, request)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	wins, collisions := 0, 0
	for err := range results {
		if err == nil {
			wins++
			continue
		}
		var exists *EvidenceExistsError
		if errors.As(err, &exists) {
			collisions++
			continue
		}
		t.Fatalf("exclusive evidence race error = %v", err)
	}
	if wins != 1 || collisions != writers-1 {
		t.Fatalf("exclusive evidence race wins=%d collisions=%d", wins, collisions)
	}
}
