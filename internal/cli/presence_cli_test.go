package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func TestRunPresenceListSynthesizesUserHumanEntry(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"claude", reservedHumanHandle} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	writeKnownAgentsConfig(t, root, []string{"claude"})
	if err := presence.Write(root, presence.New("claude", "busy", "reviewing", time.Now())); err != nil {
		t.Fatalf("write claude presence: %v", err)
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

	var claude, user *struct {
		Handle             string `json:"handle"`
		Status             string `json:"status"`
		Kind               string `json:"kind"`
		PresenceApplicable bool   `json:"presence_applicable"`
		LastSeen           string `json:"last_seen,omitempty"`
		Note               string `json:"note,omitempty"`
	}
	for i := range items {
		item := &items[i]
		switch item.Handle {
		case "claude":
			claude = item
		case reservedHumanHandle:
			user = item
		}
	}
	if claude == nil || user == nil {
		t.Fatalf("expected claude and user presence items: %#v", items)
	}
	if claude.Kind != "agent" || !claude.PresenceApplicable || claude.Status != "busy" || claude.LastSeen == "" {
		t.Fatalf("claude = %+v, want agent presence item", *claude)
	}
	if user.Kind != "human" || user.PresenceApplicable || user.Status != "human" || user.LastSeen != "" {
		t.Fatalf("user = %+v, want synthesized human item without last_seen", *user)
	}
}

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
