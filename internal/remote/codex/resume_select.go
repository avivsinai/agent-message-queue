package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The selection below copies the requests and client checks of the Codex
// TUI's `resume --last` and `resume <name>` on the shared daemon, read in
// codex-cli 0.160.1 through 0.161.0: tui/src/lib.rs
// lookup_latest_session_target_with_app_server and
// latest_session_lookup_params, tui/src/named_session_lookup.rs lookup, and
// tui/src/app_server_session/provider_selection.rs history_model_provider.

// ResumeQuery is one `codex resume --last` or `codex resume <name>`.
type ResumeQuery struct {
	// Name selects the thread by its label; empty selects the most recent.
	Name string
	// Cwd is the most recent thread's exact cwd; empty matches any (--all).
	Cwd string
	// ConfigCwd is the directory the provider is read for.
	ConfigCwd string
	// IncludeNonInteractive adds exec and app-server threads to --last.
	IncludeNonInteractive bool
	// CodexHome holds the sessions a named thread must live under.
	CodexHome string
}

// ErrNoResumeThread means Codex would find no thread for the query.
var ErrNoResumeThread = errors.New("no saved Codex session matches")

// ErrProfileProvider means a Codex profile sets the model provider: there
// the TUI filters history by its local configuration, which AMQ does not read.
var ErrProfileProvider = errors.New("a Codex profile sets model_provider")

var uuidRe = regexp.MustCompile(`^(?i:urn:uuid:)?(\{[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{32})$`)

// ParsesAsUUID reports whether Codex reads s as a thread id rather than a
// name (Uuid::parse_str: hyphenated, simple, braced or urn, any case).
func ParsesAsUUID(s string) bool { return uuidRe.MatchString(s) }

type resumeThread struct {
	ID          string  `json:"id"`
	Name        *string `json:"name"`
	Preview     string  `json:"preview"`
	Path        *string `json:"path"`
	HistoryMode string  `json:"historyMode"`
}

func (t resumeThread) label() string {
	if t.Name != nil && strings.TrimSpace(*t.Name) != "" {
		return strings.TrimSpace(*t.Name)
	}
	return strings.TrimSpace(t.Preview)
}

type resumeThreadPage struct {
	Data       []resumeThread `json:"data"`
	NextCursor *string        `json:"nextCursor"`
}

// ResolveResumeThread selects the thread Codex would resume for q on the
// daemon at sock, over one connection, and returns its id.
func ResolveResumeThread(ctx context.Context, sock string, q ResumeQuery) (string, error) {
	client, err := Dial(sock, Handlers{})
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()
	if err := initializeAMQ(ctx, client); err != nil {
		return "", err
	}
	provider, err := historyProvider(ctx, client, q.ConfigCwd)
	if err != nil {
		return "", err
	}
	if q.Name != "" {
		return threadByName(ctx, client, q, provider)
	}
	return latestThread(ctx, client, q, provider)
}

// historyProvider is the provider the TUI filters history by: config/read's
// model_provider, "openai" by default, and none from a daemon without
// config/read (config_update.rs read_effective_config_if_supported).
func historyProvider(ctx context.Context, client *Client, cwd string) ([]string, error) {
	var read struct {
		Config struct {
			ModelProvider *string `json:"model_provider"`
		} `json:"config"`
		Layers []struct {
			Name struct {
				Type    string  `json:"type"`
				Profile *string `json:"profile"`
			} `json:"name"`
			Config         map[string]json.RawMessage `json:"config"`
			DisabledReason *string                    `json:"disabledReason"`
		} `json:"layers"`
	}
	err := client.Call(ctx, "config/read", map[string]any{"includeLayers": true, "cwd": cwd}, &read)
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) && (rpcErr.Code == -32601 || rpcErr.Code == -32600 && strings.Contains(rpcErr.Message, "config/read") &&
		(strings.Contains(rpcErr.Message, "unknown variant") || strings.Contains(rpcErr.Message, "unknown method"))) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config/read: %w", err)
	}
	// The TUI takes its own provider when the highest enabled layer that
	// sets model_provider is a profile (provider_selection.rs
	// explicit_provider). Its local layers are not visible here; the
	// daemon's, highest first (config_manager_service.rs read), stand in.
	for _, layer := range read.Layers {
		if layer.DisabledReason != nil {
			continue
		}
		if _, ok := layer.Config["model_provider"]; ok {
			if layer.Name.Type == "user" && layer.Name.Profile != nil {
				return nil, ErrProfileProvider
			}
			break
		}
	}
	if read.Config.ModelProvider == nil {
		return []string{"openai"}, nil
	}
	return []string{*read.Config.ModelProvider}, nil
}

// latestThread is --last: the most recently updated thread, first from the
// state database, then with a rollout scan, kept only when its rollout
// exists on disk.
func latestThread(ctx context.Context, client *Client, q ResumeQuery, provider []string) (string, error) {
	kinds := []string{"cli", "vscode"}
	if q.IncludeNonInteractive {
		kinds = append(kinds, "exec", "appServer")
	}
	for _, stateDBOnly := range []bool{true, false} {
		params := map[string]any{"limit": 1, "sortKey": "updated_at", "sourceKinds": kinds, "archived": false}
		if provider != nil {
			params["modelProviders"] = provider
		}
		if q.Cwd != "" {
			params["cwd"] = q.Cwd
		}
		if stateDBOnly {
			params["useStateDbOnly"] = true
		}
		var page resumeThreadPage
		if err := client.Call(ctx, "thread/list", params, &page); err != nil {
			return "", fmt.Errorf("thread/list: %w", err)
		}
		for _, thread := range page.Data {
			if !ParsesAsUUID(thread.ID) {
				continue
			}
			if thread.Path != nil {
				if _, err := os.Stat(*thread.Path); err == nil {
					return thread.ID, nil
				}
			}
			break
		}
	}
	return "", ErrNoResumeThread
}

// threadByName is resume <name>: the one active interactive thread whose
// label is name, refused when two match or when the list spans pages.
func threadByName(ctx context.Context, client *Client, q ResumeQuery, provider []string) (string, error) {
	if strings.TrimSpace(q.Name) == "" {
		return "", ErrNoResumeThread
	}
	// Codex canonicalizes CODEX_HOME, so thread paths are under its real
	// path (utils/home-dir find_codex_home; review of #1001).
	home := q.CodexHome
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	sessions := filepath.Join(home, "sessions")
	matched, paginated := "", false
	var cursor *string
	for {
		params := map[string]any{"limit": 100, "sortKey": "updated_at", "sourceKinds": []string{"cli", "vscode"}, "archived": false}
		if provider != nil {
			params["modelProviders"] = provider
		}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		var page resumeThreadPage
		if err := client.Call(ctx, "thread/list", params, &page); err != nil {
			return "", fmt.Errorf("thread/list: %w", err)
		}
		paginated = paginated || page.NextCursor != nil
		for _, thread := range page.Data {
			if thread.label() != q.Name {
				continue
			}
			if thread.Path != nil {
				path := *thread.Path
				if !strings.HasPrefix(path, sessions+string(filepath.Separator)) ||
					(thread.HistoryMode == "" || thread.HistoryMode == "legacy") && !rolloutExists(path) {
					continue
				}
			}
			if !ParsesAsUUID(thread.ID) {
				return "", fmt.Errorf("app server returned invalid session id %q", thread.ID)
			}
			var read struct {
				Thread resumeThread `json:"thread"`
			}
			current := thread
			err := client.Call(ctx, "thread/read", map[string]any{"threadId": thread.ID}, &read)
			var rpcErr *rpcError
			switch {
			case err == nil:
				current = read.Thread
			case errors.As(err, &rpcErr) && rpcErr.Message == "thread not loaded: "+thread.ID:
			case errors.As(err, &rpcErr) && metadataMismatch(rpcErr.Message, thread):
				continue
			default:
				return "", fmt.Errorf("thread/read %s: %w", thread.ID, err)
			}
			if current.ID != thread.ID || current.label() != q.Name {
				continue
			}
			if matched != "" && matched != current.ID {
				return "", fmt.Errorf("multiple sessions match '%s' (including %s and %s); use a session UUID to disambiguate", q.Name, matched, current.ID)
			}
			matched = current.ID
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	switch {
	case matched == "":
		return "", ErrNoResumeThread
	case paginated:
		// Codex refuses here too: older cursors can skip a match at a page boundary.
		return "", fmt.Errorf("cannot verify a unique session label across server pages; matching session UUID: %s", matched)
	}
	return matched, nil
}

// metadataMismatch is a thread/read error that makes Codex skip the row.
func metadataMismatch(message string, thread resumeThread) bool {
	const prefix = "failed to read thread: thread-store internal error: "
	if strings.HasPrefix(message, prefix+"session metadata ") && strings.Contains(message, " belongs to thread ") &&
		strings.HasSuffix(message, ", expected "+thread.ID) {
		return true
	}
	return thread.Path != nil && strings.HasPrefix(message, prefix+"failed to read session metadata "+*thread.Path+": ")
}

// rolloutExists reports whether a legacy rollout or its compressed sibling
// is on disk (rollout/src/compression.rs existing_rollout_path).
func rolloutExists(path string) bool {
	plain := strings.TrimSuffix(path, ".zst")
	for _, candidate := range []string{plain, plain + ".zst"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}
