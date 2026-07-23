package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

type whoOwnerFilterAgent struct {
	Handle         string `json:"handle"`
	Active         bool   `json:"active"`
	PresenceSource string `json:"presence_source"`
}

type whoOwnerFilterSession struct {
	Name   string                `json:"name"`
	Agents []whoOwnerFilterAgent `json:"agents"`
}

func TestRunWhoFiltersOnlyConclusiveDeadOwnerSessions(t *testing.T) {
	baseRoot := secureTempDirForTest(t)
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	addAgent := func(session, handle string, owner *wakeOwner) {
		t.Helper()
		root := filepath.Join(baseRoot, session)
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
		if err := presence.Write(root, presence.New(handle, "busy", session, time.Now())); err != nil {
			t.Fatal(err)
		}
		if owner != nil {
			target := mustNewWakeTargetForTest(t, root, handle, injector, []string{"exec"})
			target.Owner = owner
			if err := writeWakeTarget(root, handle, target); err != nil {
				t.Fatal(err)
			}
		}
	}

	deadOwner := wakeOwner{PID: 5101, ProcessStart: "dead", BootID: "boot-1"}
	liveOwner := wakeOwner{PID: 5102, ProcessStart: "live", BootID: "boot-1"}
	unknownOwner := wakeOwner{PID: 5103, ProcessStart: "unknown", BootID: "boot-1"}
	mixedDeadOwner := wakeOwner{PID: 5104, ProcessStart: "mixed-dead", BootID: "boot-1"}
	addAgent("dead-only", "codex", &deadOwner)
	addAgent("live", "codex", &liveOwner)
	addAgent("unknown", "codex", &unknownOwner)
	addAgent("legacy", "codex", nil)
	addAgent("mixed", "codex", &mixedDeadOwner)
	addAgent("mixed", "claude", nil)

	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case liveOwner.PID:
			return wakeProcessInfo{
				PID: pid, Running: true, StartToken: liveOwner.ProcessStart, BootID: liveOwner.BootID,
			}
		case unknownOwner.PID:
			return wakeProcessInfo{PID: pid, Running: true, BootID: unknownOwner.BootID}
		default:
			return wakeProcessInfo{PID: pid}
		}
	})

	normal := readWhoOwnerFilterSessions(t, baseRoot)
	if _, ok := normal["dead-only"]; ok {
		t.Fatalf("normal discovery included conclusively dead session: %#v", normal["dead-only"])
	}
	for _, name := range []string{"legacy", "live", "mixed", "unknown"} {
		if _, ok := normal[name]; !ok {
			t.Fatalf("normal discovery hid %s session: %#v", name, normal)
		}
	}
	mixed := indexWhoOwnerFilterAgents(normal["mixed"])
	if mixed["codex"].Active || mixed["codex"].PresenceSource != "" {
		t.Fatalf("mixed dead codex = %#v, want inactive despite recent presence", mixed["codex"])
	}
	if !mixed["claude"].Active || mixed["claude"].PresenceSource != presenceSourceRecentActivity {
		t.Fatalf("mixed legacy claude = %#v, want visible recent activity", mixed["claude"])
	}
	if got := indexWhoOwnerFilterAgents(normal["unknown"])["codex"]; !got.Active {
		t.Fatalf("unknown owner should remain visible/active from recent presence: %#v", got)
	}

	all := readWhoOwnerFilterSessions(t, baseRoot, "--all")
	deadSession, ok := all["dead-only"]
	if !ok {
		t.Fatalf("--all omitted dead session: %#v", all)
	}
	dead := indexWhoOwnerFilterAgents(deadSession)["codex"]
	if dead.Active || dead.PresenceSource != "" {
		t.Fatalf("--all dead owner = %#v, want inactive", dead)
	}
}

func readWhoOwnerFilterSessions(t *testing.T, root string, extra ...string) map[string]whoOwnerFilterSession {
	t.Helper()
	args := []string{"--root", root, "--json"}
	args = append(args, extra...)
	output, err := captureEnvStdout(t, func() error {
		return runWho(args)
	})
	if err != nil {
		t.Fatalf("runWho(%v): %v", args, err)
	}
	var sessions []whoOwnerFilterSession
	if err := json.Unmarshal([]byte(output), &sessions); err != nil {
		t.Fatalf("decode who output: %v\n%s", err, output)
	}
	indexed := make(map[string]whoOwnerFilterSession, len(sessions))
	for _, session := range sessions {
		indexed[session.Name] = session
	}
	return indexed
}

func indexWhoOwnerFilterAgents(session whoOwnerFilterSession) map[string]whoOwnerFilterAgent {
	indexed := make(map[string]whoOwnerFilterAgent, len(session.Agents))
	for _, agent := range session.Agents {
		indexed[agent.Handle] = agent
	}
	return indexed
}
