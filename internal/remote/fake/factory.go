package fake

import (
	"context"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// Factory builds a fake Runtime from a registry.FactoryConfig. The epoch is
// test-only (the validator rejects it for any other kind); the fake uses it
// for deterministic tests.
func Factory(ctx context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	return New(cfg.Target, cfg.Epoch), nil
}

func init() {
	registry.Register("fake", Factory)
}
