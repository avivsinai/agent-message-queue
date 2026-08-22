package acp

import (
	"os"
	"strings"
	"testing"
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

func TestPrepareWorkerEnvRefusesParallelAgents(t *testing.T) {
	t.Setenv("BUZZ_ACP_AGENTS", "2")
	err := PrepareWorkerEnv()
	if err == nil {
		t.Fatal("PrepareWorkerEnv succeeded for agents=2, want refusal")
	}
	if !strings.Contains(err.Error(), "agents=1") {
		t.Errorf("error %q does not name the agents=1 constraint", err)
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
