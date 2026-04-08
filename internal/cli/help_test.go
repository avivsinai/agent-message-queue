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

func TestRunHelp_Routed(t *testing.T) {
	// amq help (no path) should succeed
	err := Run([]string{"help"}, "test")
	if err != nil {
		t.Fatalf("Run(help) returned error: %v", err)
	}

	// amq help send should succeed (routes to send --help)
	err = Run([]string{"help", "send"}, "test")
	if err != nil {
		t.Fatalf("Run(help send) returned error: %v", err)
	}

	// amq help dlq should succeed
	err = Run([]string{"help", "dlq"}, "test")
	if err != nil {
		t.Fatalf("Run(help dlq) returned error: %v", err)
	}

	// amq help dlq list should succeed (nested subcommand)
	err = Run([]string{"help", "dlq", "list"}, "test")
	if err != nil {
		t.Fatalf("Run(help dlq list) returned error: %v", err)
	}

	// amq help swarm tasks should succeed
	err = Run([]string{"help", "swarm", "tasks"}, "test")
	if err != nil {
		t.Fatalf("Run(help swarm tasks) returned error: %v", err)
	}

	// amq help coop init should succeed
	err = Run([]string{"help", "coop", "init"}, "test")
	if err != nil {
		t.Fatalf("Run(help coop init) returned error: %v", err)
	}

	// amq help presence set should succeed
	err = Run([]string{"help", "presence", "set"}, "test")
	if err != nil {
		t.Fatalf("Run(help presence set) returned error: %v", err)
	}

	// amq help upgrade should succeed
	err = Run([]string{"help", "upgrade"}, "test")
	if err != nil {
		t.Fatalf("Run(help upgrade) returned error: %v", err)
	}
}

func TestRunHelp_Unknown(t *testing.T) {
	// amq help unknown should fail with exit 2
	err := Run([]string{"help", "nonexistent"}, "test")
	if err == nil {
		t.Fatal("Run(help nonexistent) should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error should mention 'unknown command', got: %v", err)
	}

	// amq help dlq nonexistent should fail with exit 2
	err = Run([]string{"help", "dlq", "nonexistent"}, "test")
	if err == nil {
		t.Fatal("Run(help dlq nonexistent) should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "unknown dlq subcommand") {
		t.Errorf("error should mention 'unknown dlq subcommand', got: %v", err)
	}

	// amq help send extra should fail (send has no subcommands)
	err = Run([]string{"help", "send", "extra"}, "test")
	if err == nil {
		t.Fatal("Run(help send extra) should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "no subcommands") {
		t.Errorf("error should mention 'no subcommands', got: %v", err)
	}

	// amq help dlq list extra should fail (too many path segments)
	err = Run([]string{"help", "dlq", "list", "extra"}, "test")
	if err == nil {
		t.Fatal("Run(help dlq list extra) should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
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

func TestUnknownSubcommand_ExitCode(t *testing.T) {
	groups := []struct {
		group string
		args  []string
	}{
		{"dlq", []string{"dlq", "nonexistent"}},
		{"swarm", []string{"swarm", "nonexistent"}},
		{"coop", []string{"coop", "nonexistent"}},
		{"presence", []string{"presence", "nonexistent"}},
	}
	for _, tt := range groups {
		t.Run(tt.group, func(t *testing.T) {
			err := Run(tt.args, "test")
			if err == nil {
				t.Fatalf("Run(%v) should return error", tt.args)
			}
			if code := GetExitCode(err); code != ExitUsage {
				t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
			}
			if !strings.Contains(err.Error(), "unknown "+tt.group+" subcommand") {
				t.Errorf("error should mention 'unknown %s subcommand', got: %v", tt.group, err)
			}
			if !strings.Contains(err.Error(), "amq "+tt.group+" --help") {
				t.Errorf("error should include help hint, got: %v", err)
			}
		})
	}
}

func TestParseFlagsError_ExitCode(t *testing.T) {
	// Simulating a bad flag should produce ExitUsage
	err := Run([]string{"send", "--nonexistent-flag"}, "test")
	if err == nil {
		t.Fatal("Run(send --nonexistent-flag) should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
}

func TestInitMissingAgents_ExitCode(t *testing.T) {
	// amq init (no --agents) should exit 2
	err := Run([]string{"init", "--root", t.TempDir()}, "test")
	if err == nil {
		t.Fatal("Run(init) without --agents should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
}

func TestCleanupMissingFlag_ExitCode(t *testing.T) {
	// amq cleanup (no --tmp-older-than) should exit 2
	err := Run([]string{"cleanup"}, "test")
	if err == nil {
		t.Fatal("Run(cleanup) without --tmp-older-than should return error")
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)", code, ExitUsage)
	}
}

func TestCommandHelp_ExitZero(t *testing.T) {
	// Every command should support --help and exit 0
	commands := []string{
		"init", "send", "list", "read", "thread",
		"cleanup", "watch", "drain", "monitor", "reply",
		"upgrade", "env", "who", "doctor", "shell-setup",
	}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			err := Run([]string{cmd, "--help"}, "test")
			if err != nil {
				t.Errorf("Run(%s --help) returned error: %v", cmd, err)
			}
		})
	}
}

func TestSubcommandGroupHelp_ExitZero(t *testing.T) {
	// Subcommand groups with no args should show help and exit 0
	groups := []string{"dlq", "coop", "swarm", "presence"}
	for _, group := range groups {
		t.Run(group, func(t *testing.T) {
			err := Run([]string{group}, "test")
			if err != nil {
				t.Errorf("Run(%s) returned error: %v", group, err)
			}
		})
	}
}
