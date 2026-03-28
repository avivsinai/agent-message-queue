package kanban

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"nhooyr.io/websocket"
)

type BridgeConfig struct {
	AgentHandle    string
	AMQRoot        string
	URL            string
	ReconnectDelay time.Duration
}

func RunBridge(ctx context.Context, cfg BridgeConfig) error {
	if err := validateBridgeConfig(cfg); err != nil {
		return err
	}
	if err := fsq.EnsureAgentDirs(cfg.AMQRoot, cfg.AgentHandle); err != nil {
		return fmt.Errorf("ensure agent dirs: %w", err)
	}

	state := newBridgeState()
	for {
		err := runBridgeSession(ctx, cfg, state)
		if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return ctx.Err()
		}

		_, _ = fmt.Fprintf(os.Stderr, "kanban bridge: %v\n", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.ReconnectDelay):
		}
	}
}

func runBridgeSession(ctx context.Context, cfg BridgeConfig, state *bridgeState) error {
	state.reset()

	conn, _, err := websocket.Dial(ctx, cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("dial websocket %s: %w", cfg.URL, err)
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		notifications, parseErr := processBridgeMessage(data, state)
		if parseErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "kanban bridge: ignoring malformed message: %v\n", parseErr)
			continue
		}
		for _, note := range notifications {
			if _, err := deliverNotification(cfg.AMQRoot, cfg.AgentHandle, note); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "kanban bridge: delivery error: %v\n", err)
			}
		}
	}
}

func processBridgeMessage(data []byte, state *bridgeState) ([]bridgeNotification, error) {
	var envelope runtimeEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}

	switch envelope.Type {
	case kanbanEventSnapshot:
		var msg snapshotMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("decode snapshot: %w", err)
		}
		state.bootstrap(msg)
		return nil, nil
	case kanbanEventWorkspaceStateUpdated:
		var msg workspaceStateUpdatedMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("decode workspace update: %w", err)
		}
		state.refreshWorkspace(msg.WorkspaceID, &msg.WorkspaceState)
		return nil, nil
	case kanbanEventTaskSessionsUpdated:
		var msg taskSessionsUpdatedMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("decode task sessions update: %w", err)
		}
		return state.applyTaskSessions(msg), nil
	case kanbanEventTaskReadyForReview:
		var msg taskReadyForReviewMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("decode task ready update: %w", err)
		}
		note := state.applyTaskReadyForReview(msg)
		if note == nil {
			return nil, nil
		}
		return []bridgeNotification{*note}, nil
	case kanbanEventTaskChatMessage:
		return nil, nil
	default:
		return nil, nil
	}
}

func validateBridgeConfig(cfg BridgeConfig) error {
	if cfg.AgentHandle == "" {
		return fmt.Errorf("agent handle is required")
	}
	if cfg.AMQRoot == "" {
		return fmt.Errorf("AMQ root is required")
	}
	if cfg.URL == "" {
		return fmt.Errorf("websocket URL is required")
	}
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("parse websocket URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return fmt.Errorf("invalid websocket URL scheme %q", parsed.Scheme)
	}
	if cfg.ReconnectDelay <= 0 {
		return fmt.Errorf("reconnect delay must be > 0")
	}
	return nil
}
