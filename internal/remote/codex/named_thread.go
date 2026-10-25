package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// ControlSocket is the managed app-server daemon's control socket under
// codexHome, checked the way discovery checks it. An error wrapping
// os.ErrNotExist means no daemon is running.
func ControlSocket(codexHome string) (string, error) {
	sock := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	return sock, checkControlSocket(sock)
}

// StartNamedThread creates a thread in cwd on the daemon at sock, names it,
// and persists it with no turn, so `codex resume <id>` opens it with the name
// already set (4ip). It returns the thread id. The daemon does not tell which
// process owns a thread, so the caller creates the thread and starts the TUI
// on it: the binding is exact by construction.
//
// Codex persists a thread only at its first turn or history write. The
// thread gets one developer message that names the session; it is part of
// the model-visible history. thread/goal/set also persists, but an active
// goal drives the model.
func StartNamedThread(ctx context.Context, sock, cwd, name string) (string, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()
	if err := initializeAMQ(ctx, client); err != nil {
		return "", err
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	startParams := map[string]any{"cwd": cwd, "historyMode": "paginated", "threadSource": "user"}
	if err := client.Call(ctx, "thread/start", startParams, &started); err != nil {
		return "", fmt.Errorf("start thread: %w", err)
	}
	id := started.Thread.ID
	if id == "" {
		return "", errors.New("start thread: no thread id")
	}
	// From here a failure leaves a thread the user never sees: archive it,
	// on a fresh deadline since ctx may be what ran out.
	ok := false
	defer func() {
		if !ok {
			actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = client.Call(actx, "thread/archive", map[string]any{"threadId": id}, nil)
		}
	}()
	if err := client.Call(ctx, "thread/name/set", map[string]any{"threadId": id, "name": name}, nil); err != nil {
		return "", fmt.Errorf("name thread %s: %w", id, err)
	}
	item := map[string]any{
		"type": "message", "role": "developer",
		"content": []map[string]any{{"type": "input_text", "text": "AMQ named this Codex session " + name + "."}},
	}
	if err := client.Call(ctx, "thread/inject_items", map[string]any{"threadId": id, "items": []any{item}}, nil); err != nil {
		return "", fmt.Errorf("persist thread %s: %w", id, err)
	}
	// Leave the idle thread with no subscriber. The TUI's thread/resume then
	// shuts it down and loads it again with the TUI's own overrides and
	// developer instructions (codex-cli 0.160 app-server
	// thread_processor.rs resume_running_thread); with a subscriber left the
	// daemon ignores those overrides.
	if err := client.Call(ctx, "thread/unsubscribe", map[string]any{"threadId": id}, nil); err != nil {
		return "", fmt.Errorf("unsubscribe thread %s: %w", id, err)
	}
	ok = true
	return id, nil
}

// initializeAMQ opens an app-server session as AMQ, with the experimental
// API on (historyMode is an experimental thread/start field).
func initializeAMQ(ctx context.Context, client *Client) error {
	initParams := map[string]any{
		"clientInfo":   map[string]string{"name": "amq", "version": Version},
		"capabilities": map[string]any{"experimentalApi": true},
	}
	if err := client.Call(ctx, "initialize", initParams, nil); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	return nil
}

// StartupState holds the raw results of the app-server requests that show
// what a TUI starting in a directory meets before its session: hooks/list
// for the directory (each hook with its trust status),
// configRequirements/read (the managed and cloud requirements), and
// config/read for the directory (the effective configuration). A result is
// nil when the app-server answered its request with an error.
type StartupState struct {
	Hooks, Requirements, Config json.RawMessage
}

// ReadStartupState reads the StartupState for cwd from one session on the
// daemon at sock. It changes nothing on the daemon.
func ReadStartupState(ctx context.Context, sock, cwd string) (StartupState, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return StartupState{}, err
	}
	defer func() { _ = client.Close() }()
	if err := initializeAMQ(ctx, client); err != nil {
		return StartupState{}, err
	}
	call := func(method string, params any) (json.RawMessage, error) {
		var result json.RawMessage
		err := client.Call(ctx, method, params, &result)
		var rpc *rpcError
		if errors.As(err, &rpc) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		return result, nil
	}
	var state StartupState
	if state.Hooks, err = call("hooks/list", map[string]any{"cwds": []string{cwd}}); err != nil {
		return StartupState{}, err
	}
	if state.Requirements, err = call("configRequirements/read", nil); err != nil {
		return StartupState{}, err
	}
	if state.Config, err = call("config/read", map[string]any{"cwd": cwd}); err != nil {
		return StartupState{}, err
	}
	return state, nil
}
