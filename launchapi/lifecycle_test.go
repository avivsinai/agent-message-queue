package launchapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestLifecycleFacadeUsesOwnedBinding(t *testing.T) {
	project := t.TempDir()
	session := filepath.Join(project, ".agent-mail", "collab")
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "meta", "config.json"), []byte(`{"version":1,"agents":["claude"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "claude"); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(session)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(session, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	backend := &publicLifecycleBackend{}
	detect := backend.Detect()
	binding := internallaunch.BindingRecord{
		Version: internallaunch.BindingVersion, Backend: detect.Profile.Backend,
		HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity,
		Profile: detect.Profile.Identity(), LaunchNonce: "75757575-7575-4575-8575-757575757575",
		Resources: internallaunch.ResourceIdentitySet{Version: internallaunch.ResourceSetVersion, Resources: []internallaunch.ResourceIdentity{
			{OpaqueID: "session:one"}, {OpaqueID: "window:one", Agent: "claude"},
		}},
	}
	lease, err := internallaunch.AcquireLease(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := internallaunch.WriteBinding(root, lease, binding); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	previous := lifecycleBackends
	lifecycleBackends = func() map[string]internallaunch.Backend { return map[string]internallaunch.Backend{"test": backend} }
	t.Cleanup(func() { lifecycleBackends = previous })
	request := InspectRequestV1{RequestVersion: RequestVersionV1, Target: TargetV1{ProjectRoot: project, SessionRoot: session, Session: "collab"}}

	before := snapshotTestTree(t, session)
	inspected, err := Inspect(context.Background(), request)
	if err != nil || inspected.ResultVersion != ResultVersionV1 || inspected.State != "present" || len(inspected.Observations) != 1 || inspected.Observations[0].Resource != "window:one" {
		t.Fatalf("Inspect = %#v, %v", inspected, err)
	}
	if after := snapshotTestTree(t, session); after != before {
		t.Fatal("public Inspect mutated the session tree")
	}
	focused, err := Focus(context.Background(), FocusRequestV1(request))
	if err != nil || focused.Outcome != "attached" || backend.focuses != 1 {
		t.Fatalf("Focus = %#v, %v focuses=%d", focused, err, backend.focuses)
	}
	closed, err := Close(context.Background(), CloseRequestV1(request))
	if err != nil || closed.Outcome != "closed" || backend.closes != 1 {
		t.Fatalf("Close = %#v, %v closes=%d", closed, err, backend.closes)
	}
	if _, err := internallaunch.LoadBinding(root); err != nil {
		t.Fatalf("public Close removed AMQ binding: %v", err)
	}

	lease, err = internallaunch.AcquireLease(root, "76767676-7676-4676-8676-767676767676")
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	evidence, err := internallaunch.WriteEvidence(root, lease, internallaunch.EvidenceWriteRequest{
		Kind: internallaunch.EvidenceProviderCapture, Handle: "claude", ObservedAt: time.Now().UTC(),
		Payload: []byte(`{"method":"thread/started","params":{"thread":{"id":"019c8a2f-2b13-7000-8000-000000000001"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationID := "019c8a2f-2b13-7000-8000-000000000001"
	if err := internallaunch.WriteConversation(root, lease, internallaunch.ConversationRecord{
		Version: internallaunch.ConversationVersion, Handle: "claude", State: internallaunch.CaptureReady,
		Identity:    internallaunch.ConversationIdentity{Provider: internallaunch.CodexProvider, ID: conversationID},
		LaunchNonce: "76767676-7676-4676-8676-767676767676", EvidenceRefs: []string{evidence.ID},
		ExecutionEvidence: &internallaunch.ConversationExecutionEvidence{
			Backend: "test", Profile: detect.Profile.Identity(), Outcome: internallaunch.OutcomeCreated,
			LaunchNonce: "76767676-7676-4676-8676-767676767676", ConversationID: conversationID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	evidencePath := internallaunch.EvidencePath(session, evidence.ID)
	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 1
	if err := os.WriteFile(evidencePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt, err := Inspect(context.Background(), request)
	if err != nil || corrupt.Outcome != "action_required" || corrupt.ReasonCode != "evidence_corrupt" || backend.focuses != 1 || backend.closes != 1 {
		t.Fatalf("corrupt evidence Inspect = %#v, %v", corrupt, err)
	}
}

type publicLifecycleBackend struct {
	focuses int
	closes  int
	closed  bool
}

func (*publicLifecycleBackend) Detect() internallaunch.DetectResult {
	profile := internallaunch.Profile{Backend: "test", Platform: "test", VersionRange: ">=1 <2", Version: 1, Capabilities: []internallaunch.Capability{
		internallaunch.CapInspect, internallaunch.CapFocus, internallaunch.CapClose,
	}}
	return internallaunch.DetectResult{
		Available: true, Profile: profile, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Effective: append([]internallaunch.Capability(nil), profile.Capabilities...),
	}
}

func (*publicLifecycleBackend) Create(internallaunch.CreateRequest) (internallaunch.CreateResult, error) {
	return internallaunch.CreateResult{Outcome: internallaunch.OutcomeUnsupported}, nil
}

func (backend *publicLifecycleBackend) Inspect(internallaunch.InspectRequest) (internallaunch.InspectResult, error) {
	if backend.closed {
		return internallaunch.InspectResult{Status: internallaunch.InspectAbsent, Evidence: "closed"}, nil
	}
	return internallaunch.InspectResult{Status: internallaunch.InspectPresent, Evidence: "owned"}, nil
}

func (backend *publicLifecycleBackend) Focus(internallaunch.FocusRequest) (internallaunch.FocusResult, error) {
	backend.focuses++
	return internallaunch.FocusResult{Outcome: internallaunch.OutcomeAttached}, nil
}

func (backend *publicLifecycleBackend) Close(internallaunch.CloseRequest) (internallaunch.CloseResult, error) {
	backend.closes++
	backend.closed = true
	return internallaunch.CloseResult{Outcome: internallaunch.OutcomeClosed}, nil
}
