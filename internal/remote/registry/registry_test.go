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

func TestBuildYieldsExpectedAttachmentSet(t *testing.T) {
	// Register two test factories.
	Register("test-kind-a", func(ctx context.Context, cfg FactoryConfig) (core.Attachment, error) {
		return &stubAttachment{target: cfg.Target}, nil
	})
	Register("test-kind-b", func(ctx context.Context, cfg FactoryConfig) (core.Attachment, error) {
		return &stubAttachment{target: cfg.Target}, nil
	})

	f := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "test-kind-a", Target: "alpha"},
			{Kind: "test-kind-b", Target: "beta"},
		},
	}
	atts, err := Build(context.Background(), "/tmp/root", "/tmp/state", f)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("got %d attachments, want 2", len(atts))
	}
	if atts[0].Inspect().TargetID != "alpha" {
		t.Fatalf("atts[0] target=%q, want alpha", atts[0].Inspect().TargetID)
	}
	if atts[1].Inspect().TargetID != "beta" {
		t.Fatalf("atts[1] target=%q, want beta", atts[1].Inspect().TargetID)
	}
}

func TestBuildUnknownKindIsError(t *testing.T) {
	f := manifest.File{
		SchemaVersion: manifest.SchemaVersion,
		Adapters: []manifest.Adapter{
			{Kind: "nonexistent-kind", Target: "x"},
		},
	}
	_, err := Build(context.Background(), "/tmp/root", "/tmp/state", f)
	if err == nil {
		t.Fatal("Build accepted unknown kind")
	}
	e, ok := err.(*ErrUnknownKind)
	if !ok {
		t.Fatalf("got %T, want *ErrUnknownKind", err)
	}
	if e.Kind != "nonexistent-kind" {
		t.Fatalf("kind=%q, want nonexistent-kind", e.Kind)
	}
}
