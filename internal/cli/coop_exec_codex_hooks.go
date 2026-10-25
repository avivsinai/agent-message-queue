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

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/usagetransport/command"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

const (
	// codexHooksProbeTimeout bounds one startup probe, the app-server start
	// included.
	codexHooksProbeTimeout = 5 * time.Second
	// codexHooksProbeMaxOut bounds the stdout an embedded probe keeps.
	codexHooksProbeMaxOut = 1 << 20
	// codexHooksListID and codexRequirementsReadID are the request ids of
	// hooks/list and configRequirements/read on the stdio probe.
	codexHooksListID        = 2
	codexRequirementsReadID = 3
)

// codexStartupProbe is what Codex's own app-server, asked on the backend
// the seat's TUI will use, says about two startup screens.
type codexStartupProbe struct {
	// hooksClear: hooks/list for the launch directory shows no hook to
	// review (codexHooksListClear).
	hooksClear bool
	// requirementsClear: configRequirements/read shows that no managed or
	// cloud requirement pins check_for_update_on_startup to true
	// (codexRequirementsAllowNoUpdateCheck).
	requirementsClear bool
}

// probeCodexStartup asks the app-server the seat's TUI will use for
// hooks/list for dir and configRequirements/read, in one session. The TUI
// sends the same hooks/list for its cwd before its session starts and
// reviews every untrusted or modified hook (tui/src/startup_hooks_review.rs:
// 52-82 and hooks_rpc.rs:23-56 at rust-v0.164.0). A failed, late or
// unreadable answer, and a backend AMQ cannot tell, prove nothing.
// recorded reports a resumed thread whose identity coop exec recorded;
// every other Codex seat carries the identity overrides, which make its TUI
// run its own app-server (pinCodexShellIdentity).
func probeCodexStartup(binaryPath string, args []string, wd, dir string, recorded bool) codexStartupProbe {
	codexHome, err := codexHomeDir()
	if err != nil {
		return codexStartupProbe{}
	}
	codexHome = absPath(filepath.Clean(codexHome))
	backend := codexEmbedded
	if recorded {
		r, ok := parseCodexResume(args)
		if !ok || r.unparsed {
			return codexStartupProbe{}
		}
		backend, _ = codexLaunchBackend(binaryPath, append(slices.Clone(args[:r.start]), r.opts...), wd, codexHome)
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
	defer cancel()
	var hooks, requirements []byte
	switch backend {
	case codexOnDaemon:
		sock, sockErr := codex.ControlSocket(codexHome)
		if sockErr != nil {
			return codexStartupProbe{}
		}
		hooks, requirements, err = codex.ReadStartupState(ctx, sock, dir)
	case codexEmbedded:
		hooks, requirements, err = codexEmbeddedStartupState(ctx, binaryPath, args, dir, codexHome)
	default:
		return codexStartupProbe{}
	}
	if err != nil {
		return codexStartupProbe{}
	}
	return codexStartupProbe{
		hooksClear:        hooks != nil && codexHooksListClear(hooks, dir),
		requirementsClear: requirements != nil && codexRequirementsAllowNoUpdateCheck(requirements),
	}
}

// codexEmbeddedStartupState asks hooks/list and configRequirements/read of
// a stdio app-server started the way the seat's TUI starts its own: the
// same binary, CODEX_HOME, working directory and configuration overrides,
// with a read-only sandbox and no approvals. The run is bounded, its
// process group is killed and reaped, and its stderr is discarded; its
// stdout stays here. A result is nil when its request got an error reply.
func codexEmbeddedStartupState(ctx context.Context, binaryPath string, args []string, dir, codexHome string) (hooks, requirements []byte, err error) {
	overrides, ok := codexHookProbeOverrides(args)
	if !ok {
		return nil, nil, errors.New("the seat has options a probe app-server does not take")
	}
	reply := command.Run(ctx, edge.Command{
		Path:   binaryPath,
		Args:   append(overrides, "-s", "read-only", "-a", "never", "app-server"),
		Env:    []string{"CODEX_HOME=" + codexHome},
		Stdin:  codexStartupProbeRequests(dir),
		MaxOut: codexHooksProbeMaxOut,
		Done:   func(out []byte) bool { _, _, done := codexStartupProbeReplies(out); return done },
	})
	if reply.Failure != "" {
		return nil, nil, fmt.Errorf("codex app-server: %s", reply.Failure)
	}
	hooks, requirements, done := codexStartupProbeReplies(reply.Stdout)
	if !done {
		return nil, nil, errors.New("codex app-server did not answer the startup probe")
	}
	return hooks, requirements, nil
}

// codexArgsDisableUpdateCheck reports whether args end with AMQ's own
// override that turns off Codex's startup update check
// (launch.CodexNoUpdatePromptOverride): the last -c for that key sets it
// to false exactly as AMQ writes it.
func codexArgsDisableUpdateCheck(args []string) bool {
	last := ""
	for _, value := range codexConfigOverrides(args) {
		if key, _, _ := strings.Cut(value, "="); strings.TrimSpace(key) == codexUpdateCheckKey {
			last = value
		}
	}
	return last == launch.CodexNoUpdatePromptOverride
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

// codexStartupProbeRequests is the stdin of one probe: initialize (with
// the experimental API, as AMQ opens a daemon session), the initialized
// notification, hooks/list for dir, and configRequirements/read.
func codexStartupProbeRequests(dir string) []byte {
	msgs := []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{"name": "amq", "version": codex.Version},
				"capabilities": map[string]any{"experimentalApi": true}}},
		map[string]any{"jsonrpc": "2.0", "method": "initialized"},
		map[string]any{"jsonrpc": "2.0", "id": codexHooksListID, "method": "hooks/list",
			"params": map[string]any{"cwds": []string{dir}}},
		map[string]any{"jsonrpc": "2.0", "id": codexRequirementsReadID, "method": "configRequirements/read"},
	}
	var b bytes.Buffer
	for _, m := range msgs {
		line, _ := json.Marshal(m) // maps of strings and ints always marshal
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// codexStartupProbeReplies reads the probe's stdout: done once both replies
// or an initialize error arrived, with each result, or nil for an error
// reply.
func codexStartupProbeReplies(out []byte) (hooks, requirements []byte, done bool) {
	var gotHooks, gotRequirements bool
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
		var result []byte
		if reply.Error == nil {
			result = reply.Result
		}
		switch *reply.ID {
		case 1:
			if reply.Error != nil {
				return nil, nil, true
			}
		case codexHooksListID:
			hooks, gotHooks = result, true
		case codexRequirementsReadID:
			requirements, gotRequirements = result, true
		}
	}
	return hooks, requirements, gotHooks && gotRequirements
}

// codexRequirementsAllowNoUpdateCheck reports whether a
// configRequirements/read result leaves check_for_update_on_startup to the
// seat's own override: no requirements object, or one whose
// checkForUpdateOnStartup is absent, null or false. Managed and cloud
// requirements can pin that key (config/src/config_requirements.rs:173 at
// rust-v0.164.0, read back by app-server
// request_processors/config_processor.rs:596). true, or a reply it cannot
// read, proves nothing.
func codexRequirementsAllowNoUpdateCheck(result []byte) bool {
	var read struct {
		Requirements *struct {
			CheckForUpdateOnStartup *bool `json:"checkForUpdateOnStartup"`
		} `json:"requirements"`
	}
	if json.Unmarshal(result, &read) != nil {
		return false
	}
	return read.Requirements == nil || read.Requirements.CheckForUpdateOnStartup == nil || !*read.Requirements.CheckForUpdateOnStartup
}

// codexHooksListClear reports whether a hooks/list result shows no hook to
// review for dir: its entry for dir, or its only entry, lists every hook as
// trusted or managed, or none. Codex reviews a hook only when it is
// untrusted or modified (tui/src/hooks_rpc.rs:51-55 hook_needs_review at
// rust-v0.164.0), and the status has only those four values
// (app-server-protocol/src/protocol/v2/hook.rs:62-64); a missing status or
// any other value proves nothing.
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
		if hook.TrustStatus != "trusted" && hook.TrustStatus != "managed" {
			return false
		}
	}
	return true
}
