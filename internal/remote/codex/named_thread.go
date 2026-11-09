package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ErrThreadNotStarted proves that thread/start was never sent. Once the
// request begins, a failed reply cannot establish whether a thread exists.
var ErrThreadNotStarted = errors.New("thread/start was not sent")

// ThreadStartTimeout covers the observed 30-second contention while the daemon
// loads other seats, including protocol initialization and persistence.
const ThreadStartTimeout = 60 * time.Second

// ControlSocket is the managed app-server daemon's control socket under
// codexHome, checked the way discovery checks it. An error wrapping
// os.ErrNotExist means no daemon is running.
func ControlSocket(codexHome string) (string, error) {
	sock := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	return sock, checkControlSocket(sock)
}

// StartThread creates a native thread in cwd and persists it without a turn.
// A nonempty name sets its display name; a nonempty expectedVersion requires
// that exact app-server version before creating anything. The caller owns
// binding the returned identity to the TUI it starts.
//
// The persistence write adds one developer message to model-visible history.
func StartThread(ctx context.Context, sock, cwd, name, expectedVersion string) (string, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return "", errors.Join(ErrThreadNotStarted, err)
	}
	defer func() { _ = client.Close() }()
	version, err := initializeAMQ(ctx, client)
	if err != nil {
		return "", errors.Join(ErrThreadNotStarted, err)
	}
	if expectedVersion != "" && version != expectedVersion {
		return "", fmt.Errorf("%w: codex daemon version %q does not match %q", ErrThreadNotStarted, version, expectedVersion)
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
	if name != "" {
		if err := client.Call(ctx, "thread/name/set", map[string]any{"threadId": id, "name": name}, nil); err != nil {
			return "", fmt.Errorf("name thread %s: %w", id, err)
		}
	}
	description := "AMQ started this Codex session."
	if name != "" {
		description = "AMQ named this Codex session " + name + "."
	}
	item := map[string]any{
		"type": "message", "role": "developer",
		"content": []map[string]any{{"type": "input_text", "text": description}},
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
func initializeAMQ(ctx context.Context, client *Client) (string, error) {
	initParams := map[string]any{
		"clientInfo":   map[string]string{"name": "amq", "version": Version},
		"capabilities": map[string]any{"experimentalApi": true},
	}
	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if err := client.Call(ctx, "initialize", initParams, &initialized); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	_, version, _ := strings.Cut(initialized.UserAgent, "/")
	if fields := strings.Fields(version); len(fields) > 0 {
		return fields[0], nil
	}
	return "", nil
}

// StartupState holds the raw results of the app-server requests that show
// what a TUI starting in a directory meets before its session: hooks/list
// for the directory (each hook with its trust status),
// configRequirements/read (the managed and cloud requirements), and
// config/read for the directory (the effective configuration). A result is
// nil when the app-server answered its request with an error.
type StartupState struct {
	Hooks, Requirements, Config json.RawMessage
	Version                     string
}

// ReadStartupState reads the StartupState for cwd from one session on the
// daemon at sock. It changes nothing on the daemon.
func ReadStartupState(ctx context.Context, sock, cwd string) (StartupState, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return StartupState{}, err
	}
	defer func() { _ = client.Close() }()
	version, err := initializeAMQ(ctx, client)
	if err != nil {
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
	state := StartupState{Version: version}
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

// ReadThread reads an existing thread's working directory on one session
// whose app-server reports expectedVersion, so the version and the thread
// come from the same daemon. It changes nothing on the daemon.
func ReadThread(ctx context.Context, sock, thread, expectedVersion string) (string, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()
	version, err := initializeAMQ(ctx, client)
	if err != nil {
		return "", err
	}
	if version != expectedVersion {
		return "", fmt.Errorf("codex daemon version %q does not match %q", version, expectedVersion)
	}
	var read struct {
		Thread struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"thread"`
	}
	if err := client.Call(ctx, "thread/read", map[string]any{"threadId": thread}, &read); err != nil {
		return "", fmt.Errorf("thread/read %s: %w", thread, err)
	}
	if read.Thread.ID != thread || read.Thread.Cwd == "" {
		return "", fmt.Errorf("thread/read %s returned no working directory for it", thread)
	}
	return read.Thread.Cwd, nil
}
