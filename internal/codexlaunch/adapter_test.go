//go:build darwin || linux

package codexlaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/coder/websocket"
)

// Pro S1 finding 5: recovery could publish routing under a different
// CODEX_HOME containing the same thread ID and cwd as the acquired thread.
func TestNativeLaunchPublishesOnlyItsAcquiredEndpoint(t *testing.T) {
	for _, switchHome := range []bool{false, true} {
		t.Run(strconv.FormatBool(switchHome), func(t *testing.T) {
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
			home := nativeDaemon(t, project)
			t.Setenv("CODEX_HOME", home)
			executable := nativeProvider(t)
			amq, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			nonce := "96969696-9696-4969-8969-969696969696"
			plan, err := New(executable).PlanFresh(launch.PlanRequest{Handle: "codex", LaunchNonce: nonce, ProjectRoot: project,
				SessionRoot: session, Cwd: project, AMQExecutable: amq, ResumePolicy: launch.ResumeEnabled})
			if err != nil || !plan.PreSpawnAcquire {
				t.Fatalf("native plan = %#v, %v", plan, err)
			}
			ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{Handle: plan.Handle, LaunchNonce: nonce,
				Mode: plan.AdapterMode, Provider: launch.CodexProvider, ProviderVersion: launch.CodexDaemonVersion,
				PreSpawnAcquire: true, Backend: launch.CommandsBackendName, Profile: launch.CommandsProfile().Identity(),
				ProjectRoot: project, SessionRoot: session, Cwd: project, ProviderExecutable: executable, AMQExecutable: amq,
				TargetArgv: plan.Argv, DynamicArgv: plan.DynamicArgv})
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
			routingHome := home
			envelope := launch.ExecutionEnvelope{Cwd: project, AMQExecutable: amq, ProviderExecutable: executable,
				TargetArgv: plan.Argv, Environment: env, AcquireIdentity: Acquire,
				RecordIdentity: func(acquired launch.ExecutionTicket) error {
					if switchHome {
						routingHome = nativeDaemon(t, project)
						t.Setenv("CODEX_HOME", routingHome)
					}
					return RecordIdentity(root, acquired, env)
				}}
			prepared, err := launch.PrepareExecution(root, "codex", nonce, envelope)
			const thread = "01a1166e-dd8e-77c0-bc3b-a4b7e24e91a6"
			if switchHome {
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
func nativeDaemon(t *testing.T, cwd string) string {
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
				result = map[string]any{"userAgent": "codex/0.167.0"}
			case "hooks/list":
				result = map[string]any{"data": []any{map[string]any{"cwd": cwd, "hooks": []any{}}}}
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
