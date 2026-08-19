package launchapi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestLifecycleResultsMatchPublishedSchema(t *testing.T) {
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
		Profile: detect.Profile.Identity(), LaunchNonce: "78787878-7878-4787-8787-787878787878",
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
	inspected, err := Inspect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertLiveResultMatchesPublishedSchema(t, "InspectResultV1", inspected)
	focused, err := Focus(context.Background(), FocusRequestV1(request))
	if err != nil {
		t.Fatal(err)
	}
	assertLiveResultMatchesPublishedSchema(t, "FocusResultV1", focused)
	closed, err := Close(context.Background(), CloseRequestV1(request))
	if err != nil {
		t.Fatal(err)
	}
	assertLiveResultMatchesPublishedSchema(t, "CloseResultV1", closed)
}

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
		CallerContext: map[string]string{"run_id": "run-42", "task_generation": "3"},
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
	assertLiveLifecycleResultMatchesPublishedSchema(t, "InspectResultV1", inspected)
	if !reflect.DeepEqual(inspected.CallerContext, binding.CallerContext) {
		t.Fatalf("Inspect caller context = %#v, want %#v", inspected.CallerContext, binding.CallerContext)
	}
	if after := snapshotTestTree(t, session); after != before {
		t.Fatal("public Inspect mutated the session tree")
	}
	focused, err := Focus(context.Background(), FocusRequestV1(request))
	if err != nil || focused.Outcome != "attached" || backend.focuses != 1 {
		t.Fatalf("Focus = %#v, %v focuses=%d", focused, err, backend.focuses)
	}
	assertLiveLifecycleResultMatchesPublishedSchema(t, "FocusResultV1", focused)
	closed, err := Close(context.Background(), CloseRequestV1(request))
	if err != nil || closed.Outcome != "closed" || backend.closes != 1 {
		t.Fatalf("Close = %#v, %v closes=%d", closed, err, backend.closes)
	}
	assertLiveLifecycleResultMatchesPublishedSchema(t, "CloseResultV1", closed)
	if _, err := internallaunch.LoadBinding(root); err != nil {
		t.Fatalf("public Close removed AMQ binding: %v", err)
	}
	backend.failInspectCall = backend.inspectCalls + 2
	postMutationFailure, err := Close(context.Background(), CloseRequestV1(request))
	if err != nil || postMutationFailure.Outcome != "action_required" || postMutationFailure.ReasonCode != "post_mutation_inspect_failed" ||
		postMutationFailure.Disposition != MutationDispositionCommittedV1 || postMutationFailure.BindingGeneration == "" {
		t.Fatalf("post-mutation Inspect failure = %#v, %v", postMutationFailure, err)
	}
	backend.closeErr = errors.New("close mutation failed")
	operationFailure, err := Close(context.Background(), CloseRequestV1(request))
	if err != nil || operationFailure.Outcome != "action_required" || operationFailure.ReasonCode != "mutation_uncertain" ||
		operationFailure.Disposition != MutationDispositionUncertainV1 || operationFailure.BindingGeneration == "" {
		t.Fatalf("mutation failure = %#v, %v", operationFailure, err)
	}
	bindingPath := internallaunch.BindingPath(session)
	bindingData, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptBinding := bytes.Replace(bindingData, []byte("run-42"), bytes.Repeat([]byte("x"), internallaunch.MaxCallerContextValueBytes+1), 1)
	if err := os.WriteFile(bindingPath, corruptBinding, 0o600); err != nil {
		t.Fatal(err)
	}
	corruptContext, err := Inspect(context.Background(), request)
	if err != nil || corruptContext.Outcome != "action_required" || corruptContext.ReasonCode != "caller_context_corrupt" {
		t.Fatalf("corrupt binding caller context Inspect = %#v, %v", corruptContext, err)
	}
	if err := os.WriteFile(bindingPath, bindingData, 0o600); err != nil {
		t.Fatal(err)
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
		CallerContext: binding.CallerContext,
		Payload:       []byte(`{"source":"codex_notify_v1","provider":"codex","provider_version":"0.147.0","launch_nonce":"76767676-7676-4676-8676-767676767676","handle":"claude","conversation_id":"019c8a2f-2b13-7000-8000-000000000001","cwd":"/tmp","notification":"{\"type\":\"agent-turn-complete\",\"thread-id\":\"019c8a2f-2b13-7000-8000-000000000001\",\"turn-id\":\"turn-1\",\"cwd\":\"/tmp\",\"input-messages\":[]}"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationID := "019c8a2f-2b13-7000-8000-000000000001"
	if err := internallaunch.WriteConversation(root, lease, internallaunch.ConversationRecord{
		Version: internallaunch.ConversationVersion, Handle: "claude", State: internallaunch.CaptureReady,
		Identity:        internallaunch.ConversationIdentity{Provider: internallaunch.CodexProvider, ID: conversationID},
		ProviderVersion: "0.147.0", LaunchNonce: "76767676-7676-4676-8676-767676767676", EvidenceRefs: []string{evidence.ID},
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
	withEvidence, err := Inspect(context.Background(), request)
	if err != nil || len(withEvidence.Evidence) != 1 || !reflect.DeepEqual(withEvidence.Evidence[0].CallerContext, binding.CallerContext) ||
		!reflect.DeepEqual(withEvidence.CallerContext, binding.CallerContext) {
		t.Fatalf("Inspect evidence caller context = %#v, %v", withEvidence, err)
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
	if err != nil || corrupt.Outcome != "action_required" || corrupt.ReasonCode != "evidence_corrupt" || backend.focuses != 1 || backend.closes != 3 {
		t.Fatalf("corrupt evidence Inspect = %#v, %v", corrupt, err)
	}
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	missingInspect, err := Inspect(context.Background(), request)
	if err != nil || missingInspect.Disposition != MutationDispositionNotAppliedV1 || missingInspect.BindingGeneration != "" {
		t.Fatalf("missing-binding Inspect = %#v, %v", missingInspect, err)
	}
	missingFocus, err := Focus(context.Background(), FocusRequestV1(request))
	if err != nil || missingFocus.Disposition != MutationDispositionNotAppliedV1 || missingFocus.BindingGeneration != "" {
		t.Fatalf("missing-binding Focus = %#v, %v", missingFocus, err)
	}
	missingClose, err := Close(context.Background(), CloseRequestV1(request))
	if err != nil || missingClose.Disposition != MutationDispositionNotAppliedV1 || missingClose.BindingGeneration != "" {
		t.Fatalf("missing-binding Close = %#v, %v", missingClose, err)
	}
}

type publicLifecycleBackend struct {
	focuses         int
	closes          int
	closed          bool
	closeErr        error
	inspectCalls    int
	failInspectCall int
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
	backend.inspectCalls++
	if backend.inspectCalls == backend.failInspectCall {
		return internallaunch.InspectResult{}, errors.New("inspect timeout")
	}
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
	if backend.closeErr != nil {
		return internallaunch.CloseResult{}, backend.closeErr
	}
	backend.closed = true
	return internallaunch.CloseResult{Outcome: internallaunch.OutcomeClosed}, nil
}
