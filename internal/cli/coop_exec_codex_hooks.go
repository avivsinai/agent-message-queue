//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/usagetransport/command"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

const (
	// codexHooksProbeTimeout bounds one hooks/list probe, the app-server
	// start included.
	codexHooksProbeTimeout = 5 * time.Second
	// codexHooksProbeMaxOut bounds the stdout an embedded probe keeps.
	codexHooksProbeMaxOut = 1 << 20
	// codexHooksListID is the request id of hooks/list on the stdio probe.
	codexHooksListID = 2
)

// codexHookReviewClear reports whether Codex's own hooks/list, asked on the
// backend the TUI will use, shows no hook to review for dir: every hook
// trusted, or none. The TUI sends the same request for its cwd before its
// session starts and reviews every untrusted or modified hook
// (tui/src/startup_hooks_review.rs:52-82 and hooks_rpc.rs:23-56 at
// rust-v0.164.0). A failed, late or unreadable answer, a backend AMQ cannot
// tell, and any other status prove nothing. recorded reports a resumed
// thread whose identity coop exec recorded; every other Codex seat carries
// the identity overrides, which make its TUI run its own app-server
// (pinCodexShellIdentity).
func codexHookReviewClear(binaryPath string, args []string, wd, dir string, recorded bool) bool {
	codexHome, err := codexHomeDir()
	if err != nil {
		return false
	}
	codexHome = absPath(filepath.Clean(codexHome))
	backend := codexEmbedded
	if recorded {
		r, ok := parseCodexResume(args)
		if !ok || r.unparsed {
			return false
		}
		backend, _ = codexLaunchBackend(binaryPath, append(slices.Clone(args[:r.start]), r.opts...), wd, codexHome)
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
	defer cancel()
	var result []byte
	switch backend {
	case codexOnDaemon:
		sock, sockErr := codex.ControlSocket(codexHome)
		if sockErr != nil {
			return false
		}
		result, err = codex.ListHooks(ctx, sock, dir)
	case codexEmbedded:
		result, err = codexEmbeddedHooksList(ctx, binaryPath, args, dir, codexHome)
	default:
		return false
	}
	return err == nil && codexHooksListClear(result, dir)
}

// codexEmbeddedHooksList asks hooks/list of a stdio app-server started the
// way the seat's TUI starts its own: the same binary, CODEX_HOME, working
// directory and configuration overrides, with a read-only sandbox and no
// approvals. The run is bounded, its process group is killed and reaped,
// and its stderr is discarded; its stdout stays here.
func codexEmbeddedHooksList(ctx context.Context, binaryPath string, args []string, dir, codexHome string) ([]byte, error) {
	overrides, ok := codexHookProbeOverrides(args)
	if !ok {
		return nil, errors.New("the seat has options a probe app-server does not take")
	}
	reply := command.Run(ctx, edge.Command{
		Path:   binaryPath,
		Args:   append(overrides, "-s", "read-only", "-a", "never", "app-server"),
		Env:    []string{"CODEX_HOME=" + codexHome},
		Stdin:  codexHooksListRequests(dir),
		MaxOut: codexHooksProbeMaxOut,
		Done:   func(out []byte) bool { _, done := codexHooksListReply(out); return done },
	})
	if reply.Failure != "" {
		return nil, fmt.Errorf("codex app-server: %s", reply.Failure)
	}
	result, done := codexHooksListReply(reply.Stdout)
	if !done || result == nil {
		return nil, errors.New("codex app-server gave no hooks/list result")
	}
	return result, nil
}

// codexHookProbeOverrides returns the seat's -c, --enable and --disable
// options, which `codex app-server` applies as the TUI does (root
// config_overrides and feature_toggles, cli/src/main.rs at rust-v0.164.0).
// --profile does not apply to app-server (profile_v2_for_subcommand), so a
// seat with one is not probed.
func codexHookProbeOverrides(args []string) ([]string, bool) {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flag, value, inline := strings.Cut(arg, "=")
		switch {
		case flag == "-p" || flag == "--profile":
			return nil, false
		case flag == "-c" || flag == "--config" || flag == "--enable" || flag == "--disable":
			if !inline {
				if i+1 >= len(args) {
					return nil, false
				}
				i++
				value = args[i]
			}
			out = append(out, flag, value)
		case strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--"):
			out = append(out, "-c", arg[2:])
		case codexValueOptions[flag] && !inline:
			i++
		}
	}
	return out, true
}

// codexHooksListRequests is the stdin of one probe: initialize, the
// initialized notification, and hooks/list for dir.
func codexHooksListRequests(dir string) []byte {
	msgs := []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{"name": "amq", "version": codex.Version}}},
		map[string]any{"jsonrpc": "2.0", "method": "initialized"},
		map[string]any{"jsonrpc": "2.0", "id": codexHooksListID, "method": "hooks/list",
			"params": map[string]any{"cwds": []string{dir}}},
	}
	var b bytes.Buffer
	for _, m := range msgs {
		line, _ := json.Marshal(m) // maps of strings and ints always marshal
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// codexHooksListReply reads the probe's stdout: done once the hooks/list
// reply or an initialize error arrived, with the hooks/list result, or nil
// for an error reply.
func codexHooksListReply(out []byte) (result []byte, done bool) {
	for _, line := range bytes.Split(out, []byte("\n")) {
		var reply struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &reply) != nil || reply.Method != "" || reply.ID == nil {
			continue
		}
		switch {
		case *reply.ID == 1 && reply.Error != nil:
			return nil, true
		case *reply.ID == codexHooksListID && reply.Error != nil:
			return nil, true
		case *reply.ID == codexHooksListID:
			return reply.Result, true
		}
	}
	return nil, false
}

// codexHooksListClear reports whether a hooks/list result shows no hook to
// review for dir: its entry for dir, or its only entry, lists every hook as
// trusted, or none. Codex also reviews no managed hook, but only trusted is
// taken as evidence here.
func codexHooksListClear(result []byte, dir string) bool {
	type entry struct {
		Cwd   string `json:"cwd"`
		Hooks *[]struct {
			TrustStatus string `json:"trustStatus"`
		} `json:"hooks"`
	}
	var list struct {
		Data []entry `json:"data"`
	}
	if json.Unmarshal(result, &list) != nil {
		return false
	}
	at := slices.IndexFunc(list.Data, func(e entry) bool { return e.Cwd == dir })
	if at < 0 && len(list.Data) == 1 {
		at = 0
	}
	if at < 0 || list.Data[at].Hooks == nil {
		return false
	}
	for _, hook := range *list.Data[at].Hooks {
		if hook.TrustStatus != "trusted" {
			return false
		}
	}
	return true
}
