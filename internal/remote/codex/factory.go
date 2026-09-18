package codex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// config is the adapter-specific config block for a codex manifest entry.
type config struct {
	Socket  string `json:"socket"`
	Thread  string `json:"thread"`
	Approve bool   `json:"approve"`
}

// Factory builds a codex Attachment from a registry.FactoryConfig. It is a
// thin wrapper over Attach: the attachment is built BEFORE the connection so
// handlers install as Dial arguments. Capabilities are declared in Inspect
// (Submit, CancelRequest, Steer, ApproveTool). No behavioral change.
func Factory(ctx context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("parse codex config: %w", err)
		}
	}
	if c.Socket == "" {
		return nil, fmt.Errorf("codex config: socket is required")
	}
	if c.Thread == "" {
		return nil, fmt.Errorf("codex config: thread is required")
	}
	att, err := Attach(c.Socket, c.Thread, WithApprovals(c.Approve))
	if err != nil {
		return nil, err
	}
	return att, nil
}

func init() {
	registry.Register("codex", Factory)
}
