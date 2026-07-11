package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func TestRunWhoDistinguishesNotifierLiveFromRecentActivity(t *testing.T) {
	root := setupPresenceSourceFixture(t)

	output, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", root, "--json"})
	})
	if err != nil {
		t.Fatalf("runWho json: %v", err)
	}
	var sessions []struct {
		Agents []struct {
			Handle         string `json:"handle"`
			Active         bool   `json:"active"`
			PresenceSource string `json:"presence_source"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(output), &sessions); err != nil {
		t.Fatalf("unmarshal who json: %v\n%s", err, output)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions=%#v, want one", sessions)
	}
	agents := make(map[string]struct {
		Active bool
		Source string
	})
	for _, agent := range sessions[0].Agents {
		agents[agent.Handle] = struct {
			Active bool
			Source string
		}{Active: agent.Active, Source: agent.PresenceSource}
	}
	if got := agents["notifier"]; !got.Active || got.Source != "notifier_live" {
		t.Fatalf("notifier=%#v, want active notifier_live", got)
	}
	if got := agents["recent"]; !got.Active || got.Source != "recent_activity" {
		t.Fatalf("recent=%#v, want active recent_activity", got)
	}
	if got := agents["unverified"]; !got.Active || got.Source != "recent_activity" {
		t.Fatalf("unverified=%#v, want recent_activity without notifier claim", got)
	}
	if got := agents["stale"]; got.Active || got.Source != "" {
		t.Fatalf("stale=%#v, want inactive without source", got)
	}

	text, err := captureEnvStdout(t, func() error {
		return runWho([]string{"--root", root})
	})
	if err != nil {
		t.Fatalf("runWho text: %v", err)
	}
	for _, want := range []string{
		"notifier  active (notifier_live)",
		"recent  active (recent_activity)",
		"unverified  active (recent_activity)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("who text missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"consumer_live", "consumption"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("who text must not claim %q:\n%s", forbidden, text)
		}
	}
}

func TestDoctorOpsDistinguishesNotifierLiveFromRecentActivity(t *testing.T) {
	root := setupPresenceSourceFixture(t)
	result := runOpsChecks(root, "test", false)

	agents := make(map[string]opsAgent)
	for _, agent := range result.Agents {
		agents[agent.Handle] = agent
	}
	if got := agents["notifier"].PresenceSource; got != "notifier_live" {
		t.Fatalf("notifier presence_source=%q, want notifier_live", got)
	}
	if got := agents["recent"].PresenceSource; got != "recent_activity" {
		t.Fatalf("recent presence_source=%q, want recent_activity", got)
	}
	if got := agents["unverified"].PresenceSource; got != "recent_activity" {
		t.Fatalf("unverified presence_source=%q, want recent_activity", got)
	}
	if got := agents["stale"].PresenceSource; got != "" {
		t.Fatalf("stale presence_source=%q, want empty", got)
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal ops result: %v", err)
	}
	if !strings.Contains(string(data), `"presence_source":"notifier_live"`) ||
		!strings.Contains(string(data), `"presence_source":"recent_activity"`) {
		t.Fatalf("doctor JSON missing presence sources: %s", data)
	}

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_SESSION", "")
	text, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--ops"})
	})
	if err != nil {
		t.Fatalf("runDoctor text: %v", err)
	}
	for agent, source := range map[string]string{
		"notifier": "notifier_live",
		"recent":   "recent_activity",
	} {
		line := lineWithPrefix(text, "  "+agent+":")
		if !strings.Contains(line, ", source "+source) {
			t.Fatalf("doctor line for %s missing source %q: %q\n%s", agent, source, line, text)
		}
	}
}

func lineWithPrefix(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func setupPresenceSourceFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(secureTempDirForTest(t), "collab")
	agents := []string{"notifier", "recent", "stale", "unverified"}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  agents,
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	now := time.Now()
	for _, item := range []struct {
		agent  string
		seenAt time.Time
	}{
		{agent: "notifier", seenAt: now.Add(-time.Hour)},
		{agent: "recent", seenAt: now},
		{agent: "stale", seenAt: now.Add(-time.Hour)},
		{agent: "unverified", seenAt: now},
	} {
		if err := presence.Write(root, presence.New(item.agent, "active", "", item.seenAt)); err != nil {
			t.Fatalf("write %s presence: %v", item.agent, err)
		}
	}

	const pid = 4242
	const processStart = "verified-start"
	const bootID = "verified-boot"
	args := []string{"amq", "wake", "--root", root, "--me", "notifier"}
	writeWakeLockForTest(t, root, "notifier", wakeLock{
		PID:          pid,
		ProcessStart: processStart,
		BootID:       bootID,
		Executable:   "amq",
		Args:         args,
	})
	const unverifiedPID = 4343
	writeWakeLockForTest(t, root, "unverified", wakeLock{
		PID:        unverifiedPID,
		Executable: "amq",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		if gotPID == unverifiedPID {
			return wakeProcessInfo{
				PID:        gotPID,
				Running:    true,
				Executable: "/usr/local/bin/amq",
			}
		}
		if gotPID != pid {
			return wakeProcessInfo{PID: gotPID}
		}
		return wakeProcessInfo{
			PID:        gotPID,
			Running:    true,
			StartToken: processStart,
			BootID:     bootID,
			Executable: "/usr/local/bin/amq",
			Args:       args,
		}
	})
	return root
}
