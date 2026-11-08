//go:build darwin || linux

package codexlaunch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/coder/websocket"
)

// Pro S1 finding 5: recovery could publish routing under a different
// CODEX_HOME containing the same thread ID and cwd as the acquired thread.
func TestNativeLaunchPublishesOnlyItsAcquiredEndpoint(t *testing.T) {
	for _, scenario := range []string{"publish", "switch_home", "pre_start_version_failure", "wrong_base_root", "hook_review", "recovered_embedded_environment"} {
		t.Run(scenario, func(t *testing.T) {
			for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
				t.Setenv(key, "")
				_ = os.Unsetenv(key)
			}
			t.Setenv("HOME", t.TempDir())
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			session := filepath.Join(project, "queue")
			if err := fsq.EnsureRootDirs(session); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(session, "codex"); err != nil {
				t.Fatal(err)
			}
			identity, err := fsq.SnapshotDeliveryRoot(session)
			if err != nil {
				t.Fatal(err)
			}
			root, err := fsq.OpenDeliveryRoot(session, identity)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			// Claude F1: use the real pre-thread/start protocol failure from
			// the reviewer wedge proof, then exercise a later generation.
			hooksTrusted := new(atomic.Bool)
			hooksTrusted.Store(true)
			home := nativeDaemon(t, project, nativeDaemonOptions{initializeVersion: func(sequence int32) string {
				if scenario == "pre_start_version_failure" && sequence == 3 {
					return "0.168.0"
				}
				return "0.167.0"
			}, hooksTrusted: hooksTrusted.Load})
			t.Setenv("CODEX_HOME", home)
			executable := nativeProvider(t)
			amq, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			nonce := "96969696-9696-4969-8969-969696969696"
			var settings []string
			var initial *launch.InitialInputRequest
			if scenario == "publish" {
				settings = []string{"--model", "gpt-native-test", "--sandbox", "workspace-write", "--ask-for-approval", "on-request", "--no-alt-screen"}
				text := "Keep the requested settings."
				sum := sha256.Sum256([]byte(text))
				initial = &launch.InitialInputRequest{Kind: launch.InitialInputArgument, Value: text, SHA256: "sha256:" + hex.EncodeToString(sum[:])}
			}
			plan, err := New(executable).PlanFresh(launch.PlanRequest{Handle: "codex", LaunchNonce: nonce, ProjectRoot: project, CommittedArgs: settings, InitialInput: initial,
				SessionRoot: session, Cwd: project, AMQExecutable: amq, ResumePolicy: launch.ResumeEnabled})
			if err != nil || !plan.PreSpawnAcquire {
				t.Fatalf("native plan = %#v, %v", plan, err)
			}
			ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{Handle: plan.Handle, LaunchNonce: nonce,
				Mode: plan.AdapterMode, Provider: launch.CodexProvider, ProviderVersion: launch.CodexDaemonVersion,
				PreSpawnAcquire: true, Backend: launch.CommandsBackendName, Profile: launch.CommandsProfile().Identity(),
				ProjectRoot: project, SessionRoot: session, Cwd: project, ProviderExecutable: executable, AMQExecutable: amq,
				TargetArgv: plan.Argv, DynamicArgv: plan.DynamicArgv, InitialInput: plan.InitialInput})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := launch.AcquireLease(root, nonce)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.LockHandles("codex"); err != nil {
				t.Fatal(err)
			}
			if err := launch.WriteExecutionTicket(root, lease, ticket); err != nil {
				t.Fatal(err)
			}
			if err := launch.WriteConversation(root, lease, launch.ConversationRecord{Version: launch.ConversationVersion,
				Handle: "codex", State: launch.CapturePending, ProviderVersion: launch.CodexDaemonVersion, LaunchNonce: nonce}); err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			env := []string{"AM_ROOT=" + session, "AM_BASE_ROOT=" + session, "AM_ME=codex", "AM_ROOT_ID=" + ticket.SessionIdentity, "AM_BASE_ROOT_ID=" + ticket.SessionIdentity}
			// Claude recovery review: inherited base-root pins must not
			// substitute another routing context for the acquired ticket.
			if scenario == "wrong_base_root" {
				env[1] = "AM_BASE_ROOT=" + t.TempDir()
			}
			routingHome := home
			firstRegistration := true
			envelope := launch.ExecutionEnvelope{Cwd: project, AMQExecutable: amq, ProviderExecutable: executable,
				TargetArgv: plan.Argv, Environment: env, AcquireIdentity: Acquire,
				RecordIdentity: func(acquired launch.ExecutionTicket) error {
					if scenario == "recovered_embedded_environment" && firstRegistration {
						firstRegistration = false
						return errors.New("interrupted before routing publication")
					}
					if scenario == "switch_home" {
						routingHome = nativeDaemon(t, project)
						t.Setenv("CODEX_HOME", routingHome)
					}
					if scenario == "hook_review" {
						hooksTrusted.Store(false)
					}
					return RecordIdentity(root, acquired, env)
				}}
			prepared, err := launch.PrepareExecution(root, "codex", nonce, envelope)
			if scenario == "recovered_embedded_environment" {
				if err == nil {
					t.Fatal("expected interrupted first execution")
				}
				t.Setenv("CODEX_EXEC_SERVER_URL", "")
				prepared, err = launch.PrepareExecution(root, "codex", nonce, envelope)
			}
			if scenario == "pre_start_version_failure" {
				if !errors.Is(err, launch.ErrIdentityNotRequested) {
					t.Fatalf("pre-start failure was classified as uncertain: %v", err)
				}
				stored, loadErr := launch.LoadExecutionTicket(root, "codex")
				if loadErr != nil || stored.State != launch.ExecutionPending {
					t.Fatalf("pre-start ticket = %#v, %v", stored, loadErr)
				}
				nextNonce := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				lease, err := launch.AcquireLease(root, nextNonce)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lease.Release() }()
				if err := lease.LockHandles("codex"); err != nil {
					t.Fatal(err)
				}
				ticket.LaunchNonce = nextNonce
				if err := launch.WriteExecutionTicket(root, lease, ticket); err != nil {
					t.Fatalf("a later launch generation cannot write its ticket: %v", err)
				}
				return
			}
			const thread = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6"
			if scenario == "recovered_embedded_environment" {
				if err == nil || !strings.Contains(err.Error(), "requires an embedded") {
					t.Fatalf("recovered executor exclusion=%v", err)
				}
				if _, lookupErr := codexidentity.Lookup(thread, home); !errors.Is(lookupErr, codexidentity.ErrNone) {
					t.Fatalf("excluded recovered routing published: %v", lookupErr)
				}
				return
			}
			if scenario == "hook_review" {
				if err == nil || !strings.Contains(err.Error(), "hook review") {
					t.Fatalf("unresolved recovered hook review=%v", err)
				}
				if _, lookupErr := codexidentity.Lookup(thread, home); !errors.Is(lookupErr, codexidentity.ErrNone) {
					t.Fatalf("routing published before hook review: %v", lookupErr)
				}
				return
			}
			if scenario == "wrong_base_root" {
				if err == nil || !strings.Contains(err.Error(), "routing base") {
					t.Fatalf("substituted routing context = %v", err)
				}
				if _, lookupErr := codexidentity.Lookup(thread, home); !errors.Is(lookupErr, codexidentity.ErrNone) {
					t.Fatalf("foreign context published: %v", lookupErr)
				}
				return
			}
			if scenario == "switch_home" {
				if err == nil || !strings.Contains(err.Error(), "endpoint changed") {
					t.Fatalf("substituted endpoint = %v", err)
				}
				if _, lookupErr := codexidentity.Lookup(thread, routingHome); !errors.Is(lookupErr, codexidentity.ErrNone) {
					t.Fatalf("foreign routing was published: %v", lookupErr)
				}
				return
			}
			if err != nil || prepared.State != launch.ExecutionAcknowledged {
				t.Fatalf("prepare = %#v, %v", prepared, err)
			}
			if scenario == "publish" {
				// S2: settings remain in the ticket-owned resume argv while only
				// the acquired thread slot is substituted at execution.
				argv, resolveErr := launch.ResolveExecutionArgv(prepared, env)
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				expected := append([]string{executable, "resume", "-C", project}, settings...)
				expected = append(expected, thread, initial.Value)
				if !slices.Equal(argv, expected) {
					t.Fatalf("native settings changed at resume: got %q want %q", argv, expected)
				}
			}
			record, err := codexidentity.Lookup(thread, home)
			if err != nil || record.Root != session || record.Me != "codex" {
				t.Fatalf("published routing = %#v, %v", record, err)
			}
		})
	}
}

// Pro S1 finding 6: inherited exclusions, including empty-but-present
// values, bypassed managed admission while the TUI selector chose embedded.
func TestInheritedExecutorExclusionsRetainEmbeddedPlan(t *testing.T) {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", nativeDaemon(t, project))
			plan, err := New(nativeProvider(t)).PlanFresh(launch.PlanRequest{Handle: "codex", LaunchNonce: "97979797-9797-4979-8979-979797979797",
				ProjectRoot: project, SessionRoot: project, Cwd: project, AMQExecutable: "/usr/bin/true", ResumePolicy: launch.ResumeEnabled})
			if err != nil || plan.PreSpawnAcquire || plan.TerminalOnlyReason != "the configured executor requires an embedded app-server" {
				t.Fatalf("excluded plan = %#v, %v", plan, err)
			}
		})
	}
}

func nativeProvider(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n'--version ') echo 'codex-cli 0.167.0';;\n'--help ') echo resume;;\n'resume --help') echo '[SESSION_ID]';;\n'login status') exit 0;;\n*) exit 1;;\nesac\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}

// The protocol boundary is the behavior under test: these are real local
// WebSocket connections, including kernel peer identity and routing writes.
type nativeDaemonOptions struct {
	initializeVersion func(int32) string
	hooksTrusted      func() bool
}

func nativeDaemon(t *testing.T, cwd string, options ...nativeDaemonOptions) string {
	var initializes atomic.Int32
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "amq-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	control := filepath.Join(home, "app-server-control")
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", cwd)), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(control, "app-server-control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &request) != nil || len(request.ID) == 0 {
				continue
			}
			result := any(map[string]any{})
			switch request.Method {
			case "initialize":
				version := "0.167.0"
				sequence := initializes.Add(1)
				if len(options) != 0 && options[0].initializeVersion != nil {
					version = options[0].initializeVersion(sequence)
				}
				result = map[string]any{"userAgent": "codex/" + version}
			case "hooks/list":
				hooks := []any{}
				if len(options) != 0 && options[0].hooksTrusted != nil && !options[0].hooksTrusted() {
					hooks = append(hooks, map[string]any{"trustStatus": "untrusted"})
				}
				result = map[string]any{"data": []any{map[string]any{"cwd": cwd, "hooks": hooks}}}
			case "configRequirements/read":
				result = map[string]any{"requirements": nil}
			case "config/read":
				result = map[string]any{"config": map[string]any{"check_for_update_on_startup": false, "features": map[string]bool{}}}
			case "thread/start", "thread/read":
				result = map[string]any{"thread": map[string]any{"id": "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6", "cwd": cwd}}
			}
			reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			if conn.Write(context.Background(), websocket.MessageText, reply) != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return home
}

// Pro S1 review R1: resume planning probed an executable after static
// containment validation had already rejected it as repository-controlled.
func TestRejectedResumeProviderIsNeverExecuted(t *testing.T) {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	project := t.TempDir()
	t.Setenv("CODEX_HOME", nativeDaemon(t, project))
	executable := filepath.Join(project, "codex")
	sentinel := filepath.Join(project, "executed")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch '"+sentinel+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := New(executable).PlanResume(launch.ResumeRequest{
		PlanRequest: launch.PlanRequest{Handle: "codex", LaunchNonce: "98989898-9898-4989-8989-989898989898",
			ProjectRoot: project, SessionRoot: project, Cwd: project, AMQExecutable: "/usr/bin/true", ResumePolicy: launch.ResumeEnabled},
		Conversation: launch.ConversationIdentity{Provider: launch.CodexProvider, ID: "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6"},
	})
	if err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Fatalf("resume refusal = %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected provider was executed: %v", err)
	}
}

// Pro S1 finding 4: Prepare omitted the wrapper while selecting native mode,
// so repeated Apply could never reproduce the retained embedded plan.
func TestPublicPrepareRetainsEmbeddedWrappedCodexPlan(t *testing.T) {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", nativeDaemon(t, project))
	executable := nativeProvider(t)
	wrapper := filepath.Join(t.TempDir(), "wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The base uses the same resolved form as the project: on macOS t.TempDir
	// is under /var, a symlink to /private/var (macOS CI on PR #1402).
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"root": base})
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	amq, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	store, err := launch.OpenTrustStoreForBase(t.TempDir(), project, base)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := launch.Prepare(context.Background(), launch.PrepareRequest{
		Target:        launch.PrepareTarget{ProjectRoot: project, BaseRoot: base, SessionRoot: filepath.Join(base, "collab"), Session: "collab"},
		SubjectSchema: launch.SubjectSchemaV2, Launcher: launch.LauncherCommands, IntentDigest: "sha256:" + strings.Repeat("a", 64),
		Participants: []launch.PrepareParticipant{{Handle: "codex", Provider: launch.CodexProvider, Executable: executable, Runnable: true, Cwd: project,
			ResumePolicy: launch.ResumeEnabled, Wrapper: &launch.Wrapper{Executable: wrapper}}},
	}, launch.PrepareDependencies{Backends: map[string]launch.Backend{launch.LauncherCommands: launch.Commands{}}, AMQPath: amq, TrustStore: store,
		AdapterFor: func(provider, executable string) launch.HarnessAdapter { return New(executable) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Participants) != 1 || prepared.Participants[0].Command == nil {
		t.Fatalf("missing wrapped plan: %#v", prepared)
	}
	argv := prepared.Participants[0].Command.Argv
	if len(argv) < 3 || argv[0] != wrapper || argv[1] != executable || argv[2] != "-c" {
		t.Fatalf("wrapper was omitted from embedded admission: %q", argv)
	}
}

// S2: Codex 0.167 deliberately selects an embedded app-server for these
// overrides. Preserve the caller's settings rather than acquire a native thread.
func TestConfigurationAndSearchSettingsRetainEmbeddedExecution(t *testing.T) {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	for _, args := range [][]string{{"-c", `approvals_reviewer="user"`}, {"-c", "model_reasoning_effort=high"}, {"--search"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", nativeDaemon(t, project))
			plan, err := New(nativeProvider(t)).PlanFresh(launch.PlanRequest{Handle: "codex", LaunchNonce: "adadadad-adad-4dad-8dad-adadadadadad",
				ProjectRoot: project, SessionRoot: project, Cwd: project, AMQExecutable: "/usr/bin/true", ResumePolicy: launch.ResumeEnabled, CommittedArgs: args})
			if err != nil || plan.PreSpawnAcquire || !strings.Contains(plan.TerminalOnlyReason, "Codex 0.167 selects an embedded") {
				t.Fatalf("unverified native settings=%#v, %v", plan, err)
			}
			if !slices.Equal(plan.Argv[1:1+len(args)], args) {
				t.Fatalf("embedded settings changed: %q", plan.Argv)
			}
		})
	}
}
