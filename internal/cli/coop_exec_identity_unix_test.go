//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/agentidentity"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// Bead agent-message-queue-t97.4 (research 1, Pro finding 7): coop exec
// --me reviewer left no record of the harness, so the handle stayed
// unregistered; the record must come from the common exec boundary, also
// with --no-wake, naming disabled and an unknown binary.
func TestCoopExecRecordsTheHarnessItStarts(t *testing.T) {
	root := filepath.Join(secureTempDirForTest(t), "queue")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sentinel := errors.New("exec sentinel")
	oldExec := coopExecProcess
	coopExecProcess = func(string, []string, []string) error { return sentinel }
	t.Cleanup(func() { coopExecProcess = oldExec })

	for _, tc := range []struct{ binary, harness string }{{"sh", "other"}, {"pi", "pi"}} {
		err := runCoopExec([]string{"--root", root, "--me", "reviewer", "--no-wake", "--named=false", "-y", tc.binary})
		if !errors.Is(err, sentinel) {
			t.Fatalf("coop exec %s: %v, want exec sentinel", tc.binary, err)
		}
		rec, err := agentidentity.Read(root, "reviewer")
		if err != nil || rec.Harness != tc.harness || rec.Wrapper != "" {
			t.Fatalf("after coop exec %s: record %+v err=%v, want harness %s", tc.binary, rec, err, tc.harness)
		}
	}
	reg := classifyAgentAt(declarationsFor(root, ""), root, "reviewer")
	if reg.Status != registrationObserved {
		t.Fatalf("reviewer registration = %q, want observed", reg.Status)
	}
}

// Review of the identity-harness branch (P1): a managed launch took the
// harness from the ticket's provider executable, which the ticket stores
// symlink-resolved, so a symlinked claude install (a versions/2.1.310
// target) and a pi behind amq-acp (a node_modules cli.js target) were
// recorded as other.
func TestManagedLaunchRecordsTheHarnessOfASymlinkedInstall(t *testing.T) {
	for _, tc := range []struct {
		ticket launch.ExecutionTicket
		want   harness.Invocation
	}{
		{launch.ExecutionTicket{
			Provider:           launch.ClaudeProvider,
			ProviderExecutable: "/home/u/.local/share/claude/versions/2.1.310",
			TargetArgv:         []string{"/home/u/.local/bin/claude"},
		}, harness.Invocation{Harness: harness.Claude}},
		{launch.ExecutionTicket{
			Provider:           launch.ACPProvider,
			ProviderExecutable: "/home/u/.npm/lib/node_modules/pi/dist/cli.js",
			Wrapper:            &launch.Wrapper{Executable: "/opt/amq/amq-acp", Args: []string{"host", "--"}},
			TargetArgv:         []string{"/opt/amq/amq-acp", "host", "--", "/home/u/.npm/bin/pi", "--acp"},
		}, harness.Invocation{Harness: harness.Pi, Wrapper: "amq-acp"}},
	} {
		if got := managedInvocation(tc.ticket); got != tc.want {
			t.Fatalf("provider %s: invocation %+v, want %+v", tc.ticket.Provider, got, tc.want)
		}
	}
}
