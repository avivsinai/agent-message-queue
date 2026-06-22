package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func TestRunWhoRendersUserAsHuman(t *testing.T) {
	baseRoot := t.TempDir()
	root := filepath.Join(baseRoot, "collab")
	for _, agent := range []string{"claude", reservedHumanHandle} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	if err := presence.Write(root, presence.New("claude", "busy", "reviewing", time.Now())); err != nil {
		t.Fatalf("write claude presence: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", root, "--json"})
	})
	if err != nil {
		t.Fatalf("runWho json: %v", err)
	}
	var sessions []struct {
		Name   string `json:"name"`
		Agents []struct {
			Handle             string `json:"handle"`
			Kind               string `json:"kind"`
			PresenceApplicable bool   `json:"presence_applicable"`
			Active             bool   `json:"active"`
			Note               string `json:"note,omitempty"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(output), &sessions); err != nil {
		t.Fatalf("unmarshal who json: %v (output: %s)", err, output)
	}
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1: %#v", len(sessions), sessions)
	}

	var claude, user *struct {
		Handle             string `json:"handle"`
		Kind               string `json:"kind"`
		PresenceApplicable bool   `json:"presence_applicable"`
		Active             bool   `json:"active"`
		Note               string `json:"note,omitempty"`
	}
	for i := range sessions[0].Agents {
		agent := &sessions[0].Agents[i]
		switch agent.Handle {
		case "claude":
			claude = agent
		case reservedHumanHandle:
			user = agent
		}
	}
	if claude == nil || user == nil {
		t.Fatalf("expected claude and user in who output: %#v", sessions[0].Agents)
	}
	if claude.Kind != "agent" || !claude.PresenceApplicable || !claude.Active {
		t.Fatalf("claude = %+v, want active agent with applicable presence", *claude)
	}
	if user.Kind != "human" || user.PresenceApplicable || user.Active {
		t.Fatalf("user = %+v, want inactive human with non-applicable presence", *user)
	}

	text, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", root})
	})
	if err != nil {
		t.Fatalf("runWho text: %v", err)
	}
	if !strings.Contains(text, "user  human") {
		t.Fatalf("text output should render user as human, got:\n%s", text)
	}
	if strings.Contains(text, "user  stale") {
		t.Fatalf("text output should not render user as stale, got:\n%s", text)
	}
}
