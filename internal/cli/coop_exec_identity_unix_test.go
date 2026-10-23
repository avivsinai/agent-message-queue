//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/agentidentity"
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
