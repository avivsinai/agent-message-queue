package claude

import (
	"context"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// ErrNotAuthorized is the refusal returned by the claude stub factory. A
// kind:claude manifest entry is a doctor-visible startup refusal, absent
// from sessions. The bead stays refusal-only until 611.2's authorized
// capture (Wave C).
var ErrNotAuthorized = fmt.Errorf("claude adapter not yet authorized (gated on 611.2 wire-capture probe)")

// Factory is the stub: it always refuses. The refusal is surfaced by doctor
// and the adapter is absent from sessions.
func Factory(ctx context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	return nil, ErrNotAuthorized
}

func init() {
	registry.Register("claude", Factory)
}
