package cli

import (
	"strings"
	"testing"
)

func TestRunHelp_TopLevel(t *testing.T) {
	// amq --help should succeed (exit 0)
	err := Run([]string{"--help"}, "test")
	if err != nil {
		t.Fatalf("Run(--help) returned error: %v", err)
	}

	// amq -h should succeed
	err = Run([]string{"-h"}, "test")
	if err != nil {
		t.Fatalf("Run(-h) returned error: %v", err)
	}

	// amq (no args) should succeed
	err = Run(nil, "test")
	if err != nil {
		t.Fatalf("Run(nil) returned error: %v", err)
	}
}

func TestRunUnknownCommand_ExitCode(t *testing.T) {
	// amq unknown should exit 2 (not 1)
	err := Run([]string{"nonexistent"}, "test")
	if err == nil {
		t.Fatal("Run(nonexistent) should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error should mention 'unknown command', got: %v", err)
	}
	if !strings.Contains(err.Error(), "amq --help") {
		t.Errorf("error should include help hint, got: %v", err)
	}
}
