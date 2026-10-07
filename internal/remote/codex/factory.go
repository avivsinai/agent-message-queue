package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	// .13: the manifest target is the identity the endpoint addresses this
	// adapter under ("sales" is addressed as "sales", not codex:<thread>). The
	// native thread id stays with the daemon protocol; only the advertised
	// target id changes. cfg.Target empty = legacy derived identity.
	opts := []Option{WithApprovals(c.Approve)}
	if cfg.Target != "" {
		opts = append(opts, WithTarget(cfg.Target))
	}
	att, err := Attach(c.Socket, c.Thread, opts...)
	if threadHasNoTurn(err) {
		return nil, fmt.Errorf("codex thread %s has no turn yet, so Codex has not saved it: send one prompt in that Codex session, then attach again: %w", c.Thread, err)
	}
	if err != nil {
		return nil, err
	}
	return att, nil
}

// threadHasNoTurn reports codex-cli 0.160's refusal to resume a loaded thread
// whose rollout Codex has not written yet: it writes the rollout at the first
// turn, and thread/resume without one fails ThreadNotFound, "no rollout
// found" (app-server thread_processor.rs). AMQ does not write into the
// user's thread to create it.
func threadHasNoTurn(err error) bool {
	var rerr *rpcError
	return errors.As(err, &rerr) && rerr.Code == -32600 && strings.Contains(rerr.Message, "no rollout found")
}

func init() {
	registry.Register("codex", Factory)
}
