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
	// codexHooksProbeTimeout bounds one startup probe, the app-server start
	// included.
	codexHooksProbeTimeout = 5 * time.Second
	// codexHooksProbeMaxOut bounds the stdout an embedded probe keeps.
	codexHooksProbeMaxOut = 1 << 20
	// codexHooksListID, codexRequirementsReadID and codexConfigReadID are
	// the request ids of hooks/list, configRequirements/read and config/read
	// on the stdio probe.
	codexHooksListID        = 2
	codexRequirementsReadID = 3
	codexConfigReadID       = 4
)

// codexStartupProbe is what Codex's own app-server, asked on the backend
// the seat's TUI will use, says about two startup screens.
type codexStartupProbe struct {
	// updateCheckOff: config/read for the launch directory shows the
	// effective check_for_update_on_startup as false (codexConfigUpdateCheckOff).
	updateCheckOff bool
	// hooksClear: hooks/list for the launch directory shows no hook to
	// review (codexHooksListClear).
	hooksClear bool
	// requirementsClear: configRequirements/read shows that no managed or
	// cloud requirement pins check_for_update_on_startup to true
	// (codexRequirementsAllowNoUpdateCheck).
	requirementsClear bool
}

// codexStartupPlan is what one startup probe needs.
type codexStartupPlan struct {
	// Binary is the absolute path of the Codex executable the seat runs.
	Binary string
	// Overrides are the seat's -c, --enable and --disable options, for an
	// embedded probe.
	Overrides []string
	// Dir is the physical cwd the TUI sends hooks/list and config/read
	// for (codexPhysicalLaunchDir).
	Dir string
	// Daemon reports a seat on the shared app-server daemon.
	Daemon bool
	// CodexHome is the seat's CODEX_HOME.
	CodexHome string
}

// probeCodexStartup asks the app-server the seat's TUI will use for
// hooks/list for dir, configRequirements/read and config/read for dir, in
// one session. The TUI
// sends the same hooks/list for its cwd before its session starts and
// reviews every untrusted or modified hook (tui/src/startup_hooks_review.rs:
// 52-82 and hooks_rpc.rs:23-56 at rust-v0.164.0). A failed, late or
// unreadable answer, and a backend AMQ cannot tell, prove nothing.
// recorded reports a seat that runs on the Codex daemon when its backend
// allows: a resumed thread whose identity coop exec records, or a fresh
// launch that coop exec names on a daemon thread;
// every other Codex seat carries the identity overrides, which make its TUI
// run its own app-server (pinCodexShellIdentity). One deadline covers
// backend discovery and the probe.
func probeCodexStartup(binaryPath string, args []string, wd, dir string, recorded bool) codexStartupProbe {
	ctx, cancel := context.WithTimeout(context.Background(), codexHooksProbeTimeout)
	defer cancel()
	plan, ok := planCodexStartupProbe(ctx, binaryPath, args, wd, dir, recorded)
	if !ok {
		return codexStartupProbe{}
	}
	return runCodexStartupProbe(ctx, plan)
}

// planCodexStartupProbe decides the backend, CODEX_HOME and overrides of
// a probe for a seat started as binaryPath with args in wd. A relative
// binary path resolves against wd, as the seat's exec resolves it. ok is
// false for a seat a probe cannot ask: an unknown backend, or options a
// probe app-server does not take.
func planCodexStartupProbe(ctx context.Context, binaryPath string, args []string, wd, dir string, recorded bool) (codexStartupPlan, bool) {
	codexHome, err := codexHomeDir()
	if err != nil {
		return codexStartupPlan{}, false
	}
	if !filepath.IsAbs(binaryPath) {
		binaryPath = filepath.Join(wd, binaryPath)
	}
	plan := codexStartupPlan{Binary: filepath.Clean(binaryPath), Dir: dir, CodexHome: absPath(filepath.Clean(codexHome))}
	if recorded {
		opts := args
		if r, ok := parseCodexResume(args); ok {
			if r.unparsed {
				return codexStartupPlan{}, false
			}
			opts = append(slices.Clone(args[:r.start]), r.opts...)
		}
		backend, _ := codexLaunchBackend(ctx, plan.Binary, opts, wd, plan.CodexHome)
		switch backend {
		case codexOnDaemon:
			plan.Daemon = true
			return plan, true
		case codexEmbedded:
		default:
			return codexStartupPlan{}, false
		}
	}
	overrides, ok := codexHookProbeOverrides(args)
	if !ok {
		return codexStartupPlan{}, false
	}
	plan.Overrides = overrides
	return plan, true
}

// runCodexStartupProbe asks the app-server plan names, within ctx.
func runCodexStartupProbe(ctx context.Context, plan codexStartupPlan) codexStartupProbe {
	var state codex.StartupState
	var err error
	if plan.Daemon {
		sock, sockErr := codex.ControlSocket(plan.CodexHome)
		if sockErr != nil {
			return codexStartupProbe{}
		}
		state, err = codex.ReadStartupState(ctx, sock, plan.Dir)
	} else {
		state, err = codexEmbeddedStartupState(ctx, plan.Binary, plan.Overrides, plan.Dir, plan.CodexHome)
	}
	if err != nil {
		return codexStartupProbe{}
	}
	return codexStartupProbe{
		updateCheckOff:    state.Config != nil && codexConfigUpdateCheckOff(state.Config),
		hooksClear:        state.Hooks != nil && codexHooksListClear(state.Hooks, plan.Dir),
		requirementsClear: state.Requirements != nil && codexRequirementsAllowNoUpdateCheck(state.Requirements),
	}
}

// codexEmbeddedStartupState asks hooks/list, configRequirements/read and
// config/read of a stdio app-server started the way the seat's TUI starts its own: the
// same binary, CODEX_HOME, working directory and configuration overrides,
// with a read-only sandbox and no approvals. The run is bounded, its
// process group is killed and reaped, and its stderr is discarded; its
// stdout stays here. A result is nil when its request got an error reply.
func codexEmbeddedStartupState(ctx context.Context, binaryPath string, overrides []string, dir, codexHome string) (codex.StartupState, error) {
	reply := command.Run(ctx, edge.Command{
		Path:   binaryPath,
		Args:   append(slices.Clone(overrides), "-s", "read-only", "-a", "never", "app-server"),
		Env:    []string{"CODEX_HOME=" + codexHome},
		Stdin:  codexStartupProbeRequests(dir),
		MaxOut: codexHooksProbeMaxOut,
		Done:   func(out []byte) bool { _, done := codexStartupProbeReplies(out); return done },
	})
	if reply.Failure != "" {
		return codex.StartupState{}, fmt.Errorf("codex app-server: %s", reply.Failure)
	}
	state, done := codexStartupProbeReplies(reply.Stdout)
	if !done {
		return codex.StartupState{}, errors.New("codex app-server did not answer the startup probe")
	}
	return state, nil
}

// codexHookProbeOverrides returns the seat's -c, --enable and --disable
// options, which `codex app-server` applies as the TUI does (root
// config_overrides and feature_toggles, cli/src/main.rs at rust-v0.164.0).
// --profile does not apply to app-server (profile_v2_for_subcommand at
// rust-v0.164.0 refuses it there), and -c profile=<name> selects a legacy
// [profiles] table, not the profile file --profile loads
// (loader_overrides_for_profile sets user_config_path), so a seat with one
// is not probed and stays held until its dialog hook runs.
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
// notification, hooks/list for dir, configRequirements/read, and
// config/read for dir.
func codexStartupProbeRequests(dir string) []byte {
	msgs := []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{"name": "amq", "version": codex.Version},
				"capabilities": map[string]any{"experimentalApi": true}}},
		map[string]any{"jsonrpc": "2.0", "method": "initialized"},
		map[string]any{"jsonrpc": "2.0", "id": codexHooksListID, "method": "hooks/list",
			"params": map[string]any{"cwds": []string{dir}}},
		map[string]any{"jsonrpc": "2.0", "id": codexRequirementsReadID, "method": "configRequirements/read"},
		map[string]any{"jsonrpc": "2.0", "id": codexConfigReadID, "method": "config/read",
			"params": map[string]any{"cwd": dir}},
	}
	var b bytes.Buffer
	for _, m := range msgs {
		line, _ := json.Marshal(m) // maps of strings and ints always marshal
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// codexStartupProbeReplies reads the probe's stdout: done once every reply
// or an initialize error arrived, with each result, or nil for an error
// reply.
func codexStartupProbeReplies(out []byte) (state codex.StartupState, done bool) {
	var gotHooks, gotRequirements, gotConfig bool
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
				return codex.StartupState{}, true
			}
		case codexHooksListID:
			state.Hooks, gotHooks = result, true
		case codexRequirementsReadID:
			state.Requirements, gotRequirements = result, true
		case codexConfigReadID:
			state.Config, gotConfig = result, true
		}
	}
	return state, gotHooks && gotRequirements && gotConfig
}

// codexConfigUpdateCheckOff reports whether a config/read result shows the
// effective check_for_update_on_startup as exactly false. Codex shows its
// update prompt only when that setting is in effect (tui/src/updates.rs:28
// and :153 at rust-v0.164.0). The effective value is read, not the seat's
// own override, because legacy managed layers rank above session flags
// (config/src/config_layer_source.rs:22-27). Absent, true, or any other
// value proves nothing.
func codexConfigUpdateCheckOff(result []byte) bool {
	var read struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(result, &read) != nil {
		return false
	}
	return string(bytes.TrimSpace(read.Config[codexUpdateCheckKey])) == "false"
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
