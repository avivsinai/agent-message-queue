package cli

import (
	"encoding/json"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func TestRunPresenceSetUserListsAsHuman(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, reservedHumanHandle); err != nil {
		t.Fatalf("EnsureAgentDirs(user): %v", err)
	}
	writeKnownAgentsConfig(t, root, []string{"claude"})

	if _, err := captureEnvStdout(t, func() error {
		return runPresenceSet([]string{"--root", root, "--me", reservedHumanHandle, "--strict", "--status", "available", "--note", "watching gates"})
	}); err != nil {
		t.Fatalf("runPresenceSet user: %v", err)
	}
	output, err := captureEnvStdout(t, func() error {
		return runPresenceList([]string{"--root", root, "--json"})
	})
	if err != nil {
		t.Fatalf("runPresenceList: %v", err)
	}

	var items []struct {
		Handle             string `json:"handle"`
		Status             string `json:"status"`
		Kind               string `json:"kind"`
		PresenceApplicable bool   `json:"presence_applicable"`
		LastSeen           string `json:"last_seen,omitempty"`
		Note               string `json:"note,omitempty"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("unmarshal presence list: %v (output: %s)", err, output)
	}
	var user *struct {
		Handle             string `json:"handle"`
		Status             string `json:"status"`
		Kind               string `json:"kind"`
		PresenceApplicable bool   `json:"presence_applicable"`
		LastSeen           string `json:"last_seen,omitempty"`
		Note               string `json:"note,omitempty"`
	}
	for i := range items {
		if items[i].Handle == reservedHumanHandle {
			user = &items[i]
			break
		}
	}
	if user == nil {
		t.Fatalf("expected user presence item: %#v", items)
	}
	if user.Kind != "human" || user.PresenceApplicable || user.Status != "available" || user.Note != "watching gates" || user.LastSeen == "" {
		t.Fatalf("user = %+v, want real human presence item marked not applicable", *user)
	}
}

// presence set rewrote presence.json from scratch and dropped the wake's
// notifier and doorbell fields, so doctor --ops lost the wake state and wake
// check stopped reporting doorbell_ready (found during PR #1152).
func TestRunPresenceSetKeepsWakeFields(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := presence.SetNotifierStatus(root, "claude", "input_recovery_required", "raw", "inspect the composer"); err != nil {
		t.Fatal(err)
	}
	wake, err := presence.Read(root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	wake.DoorbellParked = true
	wake.DoorbellAttempts = 3
	if err := presence.Write(root, wake); err != nil {
		t.Fatal(err)
	}

	if _, err := captureEnvStdout(t, func() error {
		return runPresenceSet([]string{"--root", root, "--me", "claude", "--status", "busy", "--note", "reviewing"})
	}); err != nil {
		t.Fatalf("runPresenceSet: %v", err)
	}
	got, err := presence.Read(root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "busy" || got.Note != "reviewing" ||
		got.NotifierStatus != "input_recovery_required" || got.NotifierMode != "raw" ||
		got.NotifierReason != "inspect the composer" ||
		!got.DoorbellParked || got.DoorbellAttempts != 3 {
		t.Fatalf("presence after set = %#v", got)
	}
}
