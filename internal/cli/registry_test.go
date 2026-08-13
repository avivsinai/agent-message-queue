package cli

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCommandNames(t *testing.T) {
	want := []string{
		"init",
		"setup",
		"launch",
		"send",
		"list",
		"read",
		"thread",
		"trace",
		"presence",
		"cleanup",
		"watch",
		"drain",
		"monitor",
		"reply",
		"dlq",
		"wake",
		"upgrade",
		"env",
		"coop",
		"swarm",
		"integration",
		"receipts",
		"session",
		"who",
		"route",
		"doctor",
		"shell-setup",
		"completion",
	}

	if got := commandNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commandNames() = %v, want %v", got, want)
	}
}

func TestFindCommand(t *testing.T) {
	cmd := findCommand("send")
	if cmd == nil {
		t.Fatal("findCommand(send) = nil")
	}
	if cmd.Name != "send" {
		t.Fatalf("findCommand(send).Name = %q, want %q", cmd.Name, "send")
	}
	if cmd.Handler == nil {
		t.Fatal("findCommand(send).Handler = nil")
	}

	if got := findCommand("missing"); got != nil {
		t.Fatalf("findCommand(missing) = %v, want nil", got)
	}
}

func TestFindChild(t *testing.T) {
	swarm := findCommand("swarm")
	if swarm == nil {
		t.Fatal("findCommand(swarm) = nil")
	}

	child := findChild(swarm, "bridge")
	if child == nil {
		t.Fatal("findChild(swarm, bridge) = nil")
	}
	if child.Name != "bridge" {
		t.Fatalf("findChild(swarm, bridge).Name = %q, want %q", child.Name, "bridge")
	}

	if got := findChild(swarm, "missing"); got != nil {
		t.Fatalf("findChild(swarm, missing) = %v, want nil", got)
	}
}

func TestChildNames(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{name: "presence", want: []string{"set", "list"}},
		{name: "dlq", want: []string{"list", "read", "retry", "purge"}},
		{name: "wake", want: []string{"check", "repair", "restart", "recover-owner", "retire"}},
		{name: "coop", want: []string{"init", "exec"}},
		{name: "swarm", want: []string{"list", "join", "leave", "tasks", "claim", "complete", "fail", "block", "bridge"}},
		{name: "receipts", want: []string{"list", "wait"}},
		{name: "session", want: []string{"create", "list", "resume"}},
		{name: "route", want: []string{"explain"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := findCommand(tt.name)
			if cmd == nil {
				t.Fatalf("findCommand(%s) = nil", tt.name)
			}
			if got := childNames(cmd); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("childNames(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestTopLevelUsageLines(t *testing.T) {
	lines := topLevelUsageLines()

	if !containsLine(lines, "amq - agent message queue") {
		t.Fatal("topLevelUsageLines missing title")
	}
	if !containsLine(lines, "Commands:") {
		t.Fatal("topLevelUsageLines missing Commands header")
	}
	if !containsLine(lines, "shell-setup  Output shell aliases (amc/amx/amg)") {
		t.Fatal("topLevelUsageLines missing shell-setup summary")
	}
	if !containsLine(lines, "--no-update-check") {
		t.Fatal("topLevelUsageLines missing global options")
	}
	if !containsLine(lines, "AMQ_NO_UPDATE_CHECK") {
		t.Fatal("topLevelUsageLines missing environment section")
	}
	if !containsLine(lines, "AMQ_WAKE_NO_SELF_UPGRADE") {
		t.Fatal("topLevelUsageLines missing wake self-upgrade environment control")
	}
	if !containsLine(lines, `Use "amq <command> --help" for more information about a command.`) {
		t.Fatal("topLevelUsageLines missing footer")
	}
}

func TestGroupUsageLinesSwarm(t *testing.T) {
	swarm := findCommand("swarm")
	if swarm == nil {
		t.Fatal("findCommand(swarm) = nil")
	}

	lines, err := groupUsageLines(swarm)
	if err != nil {
		t.Fatalf("groupUsageLines(swarm): %v", err)
	}

	if !containsLine(lines, "amq swarm - Claude Code Agent Teams integration") {
		t.Fatal("groupUsageLines(swarm) missing header")
	}
	if !containsLine(lines, "Subcommands:") {
		t.Fatal("groupUsageLines(swarm) missing Subcommands header")
	}
	if !containsLine(lines, "bridge    Run bridge process (sync tasks -> AMQ notifications)") {
		t.Fatal("groupUsageLines(swarm) missing bridge summary")
	}
	if !containsLine(lines, "Examples:") {
		t.Fatal("groupUsageLines(swarm) missing examples section")
	}
	if !containsLine(lines, `Use "amq swarm <subcommand> --help" for details.`) {
		t.Fatal("groupUsageLines(swarm) missing footer")
	}
}

func TestGroupUsageLinesPresence(t *testing.T) {
	presence := findCommand("presence")
	if presence == nil {
		t.Fatal("findCommand(presence) = nil")
	}

	lines, err := groupUsageLines(presence)
	if err != nil {
		t.Fatalf("groupUsageLines(presence): %v", err)
	}

	if !containsLine(lines, "amq presence - Agent presence metadata") {
		t.Fatal("groupUsageLines(presence) missing header")
	}
	if !containsLine(lines, "set   Update presence status") {
		t.Fatal("groupUsageLines(presence) missing set subcommand")
	}
	if !containsLine(lines, `amq presence set --me claude --status busy --note "reviewing PR"`) {
		t.Fatal("groupUsageLines(presence) missing example")
	}
}

func TestGroupUsageLinesWakeIncludesLifecycleCommands(t *testing.T) {
	wake := findCommand("wake")
	if wake == nil {
		t.Fatal("findCommand(wake) = nil")
	}

	lines, err := groupUsageLines(wake)
	if err != nil {
		t.Fatalf("groupUsageLines(wake): %v", err)
	}
	if !containsLine(lines, "check          Inspect wake start and restart capability without mutation") {
		t.Fatal("groupUsageLines(wake) missing check subcommand")
	}
	if !containsLine(lines, "repair         Restart a proven-stale wake from a saved inject-via target") {
		t.Fatal("groupUsageLines(wake) missing repair subcommand")
	}
	if !containsLine(lines, "recover-owner  Recover an exact owner-bound wake claim") {
		t.Fatal("groupUsageLines(wake) missing recover-owner subcommand")
	}
	if !containsLine(lines, "retire         Retire an exact managed wake and its matching retained state") {
		t.Fatal("groupUsageLines(wake) missing retire subcommand")
	}
}

func TestPrintUsageRegistry(t *testing.T) {
	output := captureRegistryStdout(t, func() error {
		return printUsageRegistry()
	})

	if !strings.Contains(output, "Commands:") {
		t.Fatalf("printUsageRegistry output missing Commands section:\n%s", output)
	}
	if !strings.Contains(output, "swarm        Claude Code Agent Teams integration") {
		t.Fatalf("printUsageRegistry output missing swarm command:\n%s", output)
	}
	if !strings.Contains(output, "session") || !strings.Contains(output, "Create, list, and resume named AMQ sessions") {
		t.Fatalf("printUsageRegistry output missing session command:\n%s", output)
	}
	if !strings.Contains(output, "Exit codes:") {
		t.Fatalf("printUsageRegistry output missing Exit codes section:\n%s", output)
	}
	if !strings.Contains(output, "6  Action required") {
		t.Fatalf("printUsageRegistry output missing exit code 6:\n%s", output)
	}
}

func TestPrintGroupUsage(t *testing.T) {
	output := captureRegistryStdout(t, func() error {
		return printGroupUsage(findCommand("coop"))
	})

	if !strings.Contains(output, "amq coop - Co-op mode for multi-agent collaboration") {
		t.Fatalf("printGroupUsage(coop) missing header:\n%s", output)
	}
	if !strings.Contains(output, "exec  Set up co-op mode and exec into an agent") {
		t.Fatalf("printGroupUsage(coop) missing exec summary:\n%s", output)
	}
	if !strings.Contains(output, `Use "amq coop <subcommand> --help" for details.`) {
		t.Fatalf("printGroupUsage(coop) missing footer:\n%s", output)
	}
}

func TestPrintGroupUsageSession(t *testing.T) {
	output := captureRegistryStdout(t, func() error {
		return printGroupUsage(findCommand("session"))
	})

	if !strings.Contains(output, "amq session - Named session lifecycle") {
		t.Fatalf("printGroupUsage(session) missing header:\n%s", output)
	}
	if !strings.Contains(output, "create  Create a named session and its roster mailboxes") {
		t.Fatalf("printGroupUsage(session) missing create:\n%s", output)
	}
	if !strings.Contains(output, "list    List sessions under the base root") {
		t.Fatalf("printGroupUsage(session) missing list:\n%s", output)
	}
	if !strings.Contains(output, "resume  Resume an existing session through launch reconciliation") {
		t.Fatalf("printGroupUsage(session) missing resume:\n%s", output)
	}
	if findChild(findCommand("session"), "resume") == nil {
		t.Fatal("session must register a resume subcommand")
	}
}

func TestPrintGroupUsageLeafCommandErrors(t *testing.T) {
	err := printGroupUsage(findCommand("send"))
	if err == nil {
		t.Fatal("printGroupUsage(send) expected error, got nil")
	}
	if !strings.Contains(err.Error(), `command "send" has no subcommands`) {
		t.Fatalf("printGroupUsage(send) error = %q", err)
	}
}

func TestUpgradeRegistryHandlerHelp(t *testing.T) {
	upgrade := findCommand("upgrade")
	if upgrade == nil {
		t.Fatal("findCommand(upgrade) = nil")
	}

	output := captureRegistryStdout(t, func() error {
		return upgrade.Handler([]string{"--help"})
	})
	if !strings.Contains(output, "Usage:\n  amq upgrade") {
		t.Fatalf("upgrade help output missing usage:\n%s", output)
	}
}

func containsLine(lines []string, needle string) bool {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func captureRegistryStdout(t *testing.T, fn func() error) string {
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

	if runErr != nil {
		t.Fatalf("captured function error: %v", runErr)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}
