package registry

import (
	"context"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// stubAttachment is a minimal core.Attachment for testing Build.
type stubAttachment struct {
	target string
}

func (s *stubAttachment) Inspect() protocol.Session {
	return protocol.Session{TargetID: s.target}
}
func (s *stubAttachment) Submit(core.BoundRequest) (core.Admission, error) {
	return core.Admission{}, nil
}
func (s *stubAttachment) Lookup(requests.Key, string) (core.Evidence, error) {
	return core.Evidence{}, nil
}
func (s *stubAttachment) CancelExact(requests.Key, string) (core.CancelEvidence, error) {
	return core.CancelEvidence{}, nil
}
func (s *stubAttachment) Respond(requests.Key, string, string, string) (protocol.Code, error) {
	return "", nil
}
func (s *stubAttachment) AcknowledgeResult(requests.Key, string, string) {}
func (s *stubAttachment) Subscribe(func(core.NativeEvent)) func()        { return func() {} }

// The test kinds are registered once per process: Register refuses a
// duplicate kind, so registering in a test body panicked under -count=2.
func init() {
	Register("test-ok", func(ctx context.Context, cfg FactoryConfig) (core.Attachment, error) {
		return &stubAttachment{target: cfg.Target}, nil
	})
	Register("test-bad", func(ctx context.Context, cfg FactoryConfig) (core.Attachment, error) {
		return nil, errTestFactoryRefusal
	})
}

// TestBuildPartialFailureReturnsRefusals (611.13 r1 recut) pins that one bad
// adapter does not take down the set: a manifest with an adapter that builds,
// one whose factory refuses, and one of an unknown kind yields one attachment
// and two typed refusals, not a fatal error.
func TestBuildPartialFailureReturnsRefusals(t *testing.T) {
	f := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "test-ok", Target: "good"},
			{Kind: "test-bad", Target: "bad"},
			{Kind: "nonexistent-kind", Target: "x"},
		},
	}
	outcomes := Build(context.Background(), "/tmp/root", "/tmp/state", f)
	if len(outcomes) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(outcomes))
	}
	if outcomes[0].Attachment == nil || outcomes[0].Refusal != nil {
		t.Fatalf("outcome[0]: expected attachment, got refusal=%v", outcomes[0].Refusal)
	}
	if outcomes[0].Attachment.Inspect().TargetID != "good" {
		t.Fatalf("outcome[0] target=%q, want good", outcomes[0].Attachment.Inspect().TargetID)
	}
	if outcomes[1].Attachment != nil {
		t.Fatal("outcome[1]: expected refusal, got attachment")
	}
	if outcomes[1].Refusal != errTestFactoryRefusal {
		t.Fatalf("outcome[1] refusal=%v, want errTestFactoryRefusal", outcomes[1].Refusal)
	}
	if outcomes[2].Attachment != nil {
		t.Fatal("outcome[2]: expected refusal, got attachment")
	}
	e, ok := outcomes[2].Refusal.(*ErrUnknownKind)
	if !ok || e.Kind != "nonexistent-kind" {
		t.Fatalf("outcome[2] refusal=%v (%T), want *ErrUnknownKind for nonexistent-kind", outcomes[2].Refusal, outcomes[2].Refusal)
	}
}

var errTestFactoryRefusal = &typedRefusal{msg: "test factory refused"}

type typedRefusal struct{ msg string }

func (e *typedRefusal) Error() string { return e.msg }
