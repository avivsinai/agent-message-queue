package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func captureCompletionStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	runErr := fn()

	_ = w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), runErr
}

func TestCompletionBash(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"bash"})
	})
	if err != nil {
		t.Fatalf("runCompletion(bash) error: %v", err)
	}

	// Must define the _amq function.
	if !strings.Contains(output, "_amq()") {
		t.Fatal("bash completion missing _amq() function")
	}
	// Must register the completion.
	if !strings.Contains(output, "complete -F _amq amq") {
		t.Fatal("bash completion missing 'complete -F _amq amq'")
	}
	// Must include top-level commands from the registry.
	for _, name := range commandNames() {
		if !strings.Contains(output, name) {
			t.Fatalf("bash completion missing command %q", name)
		}
	}
	// Must include subcommands for groups.
	for _, group := range []string{"dlq", "coop", "swarm", "presence"} {
		if !strings.Contains(output, group+")") {
			t.Fatalf("bash completion missing case for group %q", group)
		}
	}
}

func TestCompletionZsh(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"zsh"})
	})
	if err != nil {
		t.Fatalf("runCompletion(zsh) error: %v", err)
	}

	// Must start with #compdef.
	if !strings.HasPrefix(output, "#compdef amq") {
		t.Fatalf("zsh completion does not start with #compdef amq, got: %s", output[:min(60, len(output))])
	}
	// Must define the _amq function.
	if !strings.Contains(output, "_amq()") {
		t.Fatal("zsh completion missing _amq() function")
	}
	if !strings.Contains(output, `_amq "$@"`) {
		t.Fatal("zsh completion missing '_amq \"$@\"' invocation")
	}
	// Must include command descriptions in 'name:summary' format.
	if !strings.Contains(output, "'send:Send a message'") {
		t.Fatal("zsh completion missing 'send:Send a message' entry")
	}
	// Must include subcommand groups.
	for _, group := range []string{"dlq", "coop", "swarm", "presence"} {
		if !strings.Contains(output, group+")") {
			t.Fatalf("zsh completion missing case for group %q", group)
		}
	}
}

func TestCompletionFish(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"fish"})
	})
	if err != nil {
		t.Fatalf("runCompletion(fish) error: %v", err)
	}

	// Must use complete -c amq.
	if !strings.Contains(output, "complete -c amq") {
		t.Fatal("fish completion missing 'complete -c amq'")
	}
	// Must include top-level commands.
	if !strings.Contains(output, "__fish_use_subcommand") {
		t.Fatal("fish completion missing __fish_use_subcommand")
	}
	for _, name := range commandNames() {
		if !strings.Contains(output, "-a "+name) {
			t.Fatalf("fish completion missing top-level command %q", name)
		}
	}
	// Must include subcommand completions for groups.
	for _, group := range []string{"dlq", "coop", "swarm", "presence"} {
		if !strings.Contains(output, "__fish_seen_subcommand_from "+group) {
			t.Fatalf("fish completion missing subcommands for group %q", group)
		}
	}
}

func TestCompletionNoArgs(t *testing.T) {
	err := runCompletion([]string{})
	if err == nil {
		t.Fatal("runCompletion(no args) expected error, got nil")
	}
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitCodeError, got %T: %v", err, err)
	}
	if exitErr.Code != ExitUsage {
		t.Fatalf("expected exit code %d, got %d", ExitUsage, exitErr.Code)
	}
}

func TestCompletionInvalidShell(t *testing.T) {
	err := runCompletion([]string{"invalid"})
	if err == nil {
		t.Fatal("runCompletion(invalid) expected error, got nil")
	}
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitCodeError, got %T: %v", err, err)
	}
	if exitErr.Code != ExitUsage {
		t.Fatalf("expected exit code %d, got %d", ExitUsage, exitErr.Code)
	}
}

func TestCompletionBashIncludesRegistrySubcommands(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"bash"})
	})
	if err != nil {
		t.Fatalf("runCompletion(bash) error: %v", err)
	}

	// Verify subcommands come from the registry, not hardcoded.
	swarm := findCommand("swarm")
	if swarm == nil {
		t.Fatal("findCommand(swarm) = nil")
	}
	for _, child := range childNames(swarm) {
		if !strings.Contains(output, child) {
			t.Fatalf("bash completion missing swarm subcommand %q", child)
		}
	}
}

func TestCompletionZshIncludesRegistrySubcommands(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"zsh"})
	})
	if err != nil {
		t.Fatalf("runCompletion(zsh) error: %v", err)
	}

	// Verify each group's children appear in the zsh output.
	for _, groupName := range []string{"dlq", "coop", "swarm", "presence"} {
		cmd := findCommand(groupName)
		if cmd == nil {
			t.Fatalf("findCommand(%s) = nil", groupName)
		}
		for _, child := range cmd.Children {
			needle := "'" + child.Name + ":"
			if !strings.Contains(output, needle) {
				t.Fatalf("zsh completion missing %s subcommand %q", groupName, child.Name)
			}
		}
	}
}

func TestCompletionFishIncludesRegistrySubcommands(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"fish"})
	})
	if err != nil {
		t.Fatalf("runCompletion(fish) error: %v", err)
	}

	// Verify each group's children appear in the fish output.
	for _, groupName := range []string{"dlq", "coop", "swarm", "presence"} {
		cmd := findCommand(groupName)
		if cmd == nil {
			t.Fatalf("findCommand(%s) = nil", groupName)
		}
		for _, child := range cmd.Children {
			needle := "-a " + child.Name
			if !strings.Contains(output, needle) {
				t.Fatalf("fish completion missing %s subcommand %q", groupName, child.Name)
			}
		}
	}
}
