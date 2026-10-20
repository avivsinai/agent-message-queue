package acp

import (
	"os"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

func TestPrepareWorkerEnvStripsBuzzSecrets(t *testing.T) {
	t.Setenv("BUZZ_PRIVATE_KEY", "nsec-must-not-survive")
	t.Setenv("BUZZ_ACP_AGENT_COMMAND", "amq-acp")
	t.Setenv("BUZZ_RELAY_URL", "ws://localhost:3000")

	if err := PrepareWorkerEnv(); err != nil {
		t.Fatalf("PrepareWorkerEnv: %v", err)
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "BUZZ_") {
			t.Errorf("worker env still contains %s", name)
		}
	}
}

// Desktop's Create agent gives a draft parallelism 10 (harness log
// 2026-10-19): binding mode runs those workers, BYOH mode runs one.
func TestPrepareWorkerEnvRefusesParallelAgents(t *testing.T) {
	t.Setenv("BUZZ_ACP_AGENTS", "10")
	t.Setenv(EnvRemote, RemoteModeBinding)
	if err := PrepareWorkerEnv(); (err == nil) != lock.AdvisoryLockAvailable() {
		t.Fatalf("binding mode, agents=10: PrepareWorkerEnv = %v", err)
	}

	t.Setenv("BUZZ_ACP_AGENTS", "10")
	t.Setenv(EnvRemote, "")
	err := PrepareWorkerEnv()
	if err == nil {
		t.Fatal("BYOH mode: PrepareWorkerEnv succeeded for agents=10, want refusal")
	}
	if !strings.Contains(err.Error(), "BUZZ_ACP_AGENTS=1") {
		t.Errorf("error %q does not name BUZZ_ACP_AGENTS=1", err)
	}
}

func TestPrepareWorkerEnvRefusesOpenInbound(t *testing.T) {
	t.Setenv("BUZZ_ACP_RESPOND_TO", "anyone")
	err := PrepareWorkerEnv()
	if err == nil {
		t.Fatal("PrepareWorkerEnv succeeded for respond-to=anyone, want refusal")
	}
	if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("error %q does not name owner-only inbound", err)
	}
}

func TestPrepareWorkerEnvAllowsOwnerOnlySingleAgent(t *testing.T) {
	t.Setenv("BUZZ_ACP_AGENTS", "1")
	t.Setenv("BUZZ_ACP_RESPOND_TO", "owner-only")
	t.Setenv("BUZZ_PRIVATE_KEY", "nsec-must-not-survive")
	if err := PrepareWorkerEnv(); err != nil {
		t.Fatalf("PrepareWorkerEnv: %v", err)
	}
	if os.Getenv("BUZZ_PRIVATE_KEY") != "" {
		t.Fatal("BUZZ_PRIVATE_KEY survived PrepareWorkerEnv")
	}
}
