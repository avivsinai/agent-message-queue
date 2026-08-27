//go:build darwin || linux

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
)

func TestSelfUpgradeVersionProbeKillsInheritedPipeProcess(t *testing.T) {
	dir := t.TempDir()
	incumbentPath := filepath.Join(dir, "incumbent")
	candidatePath := filepath.Join(dir, "candidate")
	writeExecutableForSelfUpgradeTest(t, incumbentPath, "old image")
	if err := os.WriteFile(candidatePath, []byte("#!/bin/sh\nprintf '1.1.0\\n'\n(sleep 60) &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	incumbent := captureSelfUpgradeTestImage(t, incumbentPath, "1.0.0")
	controller := testSelfUpgradeController(dir, candidatePath, incumbent)

	previousExec := selfUpgradeExecImage
	t.Cleanup(func() { selfUpgradeExecImage = previousExec })
	selfUpgradeExecImage = func(selfupgrade.ImageEvidence, []string, []string) error {
		t.Fatal("timed-out candidate must not be executed")
		return nil
	}

	started := time.Now()
	if err := controller.maintain(context.Background()); err != nil {
		t.Fatalf("maintain() error = %v, want deferred timeout", err)
	}
	if elapsed := time.Since(started); elapsed > selfUpgradeVersionProbeLimit+2*time.Second {
		t.Fatalf("version probe took %s, want it bounded near %s", elapsed, selfUpgradeVersionProbeLimit)
	}
	if len(controller.refused) != 0 {
		t.Fatalf("timed-out candidate consumed refusal memory: %#v", controller.refused)
	}
}
