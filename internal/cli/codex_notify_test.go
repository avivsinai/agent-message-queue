package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const codexNotifyTestID = "019c8a2f-2b13-7000-8000-000000000001"

func TestCodexNotifyRecordsBeforeForwardAndIgnoresForwardFailure(t *testing.T) {
	session, root, nonce, amq := seedCodexNotifyCLI(t)
	payload := []byte(`{"type":"agent-turn-complete","thread-id":"` + codexNotifyTestID + `","turn-id":"turn-1","cwd":"` + filepath.Join(session, "work") + `","input-messages":["reply with ok"],"last-assistant-message":"ok"}`)
	original := codexNotifyForward
	t.Cleanup(func() { codexNotifyForward = original })
	wantForwardErr := errors.New("operator hook failed")
	codexNotifyForward = func(gotAMQ string, gotPayload []byte) error {
		gotIdentity, gotErr := fsq.StableFileIdentity(gotAMQ)
		wantIdentity, wantErr := fsq.StableFileIdentity(amq)
		if gotErr != nil || wantErr != nil || gotIdentity != wantIdentity || !reflect.DeepEqual(gotPayload, payload) {
			t.Fatalf("forward = %q %q", gotAMQ, gotPayload)
		}
		record, err := launch.LoadConversation(root, "codex")
		if err != nil || record.State != launch.CaptureReady || record.Identity.ID != codexNotifyTestID || len(record.EvidenceRefs) != 1 {
			t.Fatalf("conversation before forward = %#v, %v", record, err)
		}
		if _, _, err := launch.ReadEvidence(root, record.EvidenceRefs[0]); err != nil {
			t.Fatalf("evidence before forward: %v", err)
		}
		return wantForwardErr
	}
	t.Setenv(launch.InternalLaunchNonceEnv, nonce)
	if err := Run([]string{"__codex-notify", "--root", session, "--handle", "codex", string(payload)}, "test"); err != nil {
		t.Fatalf("run notify with failed operator hook: %v", err)
	}
}

func TestCodexNotifyRequiresManagedNonceEnvironment(t *testing.T) {
	t.Setenv(launch.InternalLaunchNonceEnv, "")
	err := runCodexNotify([]string{"--root", t.TempDir(), "--handle", "codex", `{}`})
	if err == nil || !strings.Contains(err.Error(), launch.InternalLaunchNonceEnv) {
		t.Fatalf("missing managed nonce error = %v", err)
	}
}

func TestForwardOperatorCodexNotifyUsesIdenticalPayload(t *testing.T) {
	home := t.TempDir()
	output := filepath.Join(t.TempDir(), "payload")
	script := filepath.Join(t.TempDir(), "notify")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$2\" > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "notify = [" + tomlQuote(script) + ", " + tomlQuote(output) + "]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	amq, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"type":"agent-turn-complete","thread-id":"` + codexNotifyTestID + `"}`)
	if err := forwardOperatorCodexNotify(amq, payload); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !reflect.DeepEqual(got, payload) {
		t.Fatalf("forwarded payload = %q, %v", got, err)
	}
}

func TestForwardOperatorCodexNotifyAbsentAndSelfAreNoOps(t *testing.T) {
	amq, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := forwardOperatorCodexNotify(amq, []byte(`{}`)); err != nil {
		t.Fatalf("absent config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("notify = ["+tomlQuote(amq)+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forwardOperatorCodexNotify(amq, []byte(`{}`)); err != nil {
		t.Fatalf("self config: %v", err)
	}
}

func seedCodexNotifyCLI(t *testing.T) (string, *fsq.DeliveryRoot, string, string) {
	t.Helper()
	project := t.TempDir()
	session := filepath.Join(t.TempDir(), "collab")
	cwd := filepath.Join(session, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
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
	t.Cleanup(func() { _ = root.Close() })
	amq, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	amq, err = filepath.EvalSymlinks(amq)
	if err != nil {
		t.Fatal(err)
	}
	session, err = filepath.EvalSymlinks(session)
	if err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(provider, []byte("provider"), 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := "78787878-7878-4787-8787-787878787878"
	notifyArgv := []string{amq, "__codex-notify", "--root", session, "--handle", "codex"}
	encoded, err := json.Marshal(notifyArgv)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{
		Handle: "codex", LaunchNonce: nonce, Mode: launch.AdapterModeCapture, Provider: launch.CodexProvider,
		ProviderVersion: "0.147.0", Backend: launch.CommandsBackendName, Profile: launch.CommandsProfile().Identity(),
		ProjectRoot: project, SessionRoot: session, Cwd: cwd, ProviderExecutable: provider, AMQExecutable: amq,
		TargetArgv: []string{provider, "-c", "notify=" + string(encoded)}, State: launch.ExecutionSpawnAttempted, Reason: "spawn_attempted",
	})
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
	if err := launch.WriteConversation(root, lease, launch.ConversationRecord{
		Version: launch.ConversationVersion, Handle: "codex", State: launch.CapturePending,
		ProviderVersion: "0.147.0", LaunchNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	return session, root, nonce, amq
}

func tomlQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
