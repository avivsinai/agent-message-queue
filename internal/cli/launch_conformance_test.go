package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

type countingLaunchBackend struct {
	launch.Backend
	creates *int
	closes  *int
}

func (b countingLaunchBackend) Create(req launch.CreateRequest) (launch.CreateResult, error) {
	*b.creates++
	return b.Backend.Create(req)
}

func (b countingLaunchBackend) Close(req launch.CloseRequest) (launch.CloseResult, error) {
	*b.closes++
	return b.Backend.Close(req)
}

func TestLaunchPathDeclaredPlanOnlyEmitsCoopExec(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	launchIsTerminal = func() bool { return true }
	launchInput = func() *bufio.Reader { return bufio.NewReader(strings.NewReader("y\n")) }

	stdout, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{}) })
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("exit=%d err=%v output=%s", GetExitCode(err), err, stdout)
	}
	if !strings.Contains(stdout, "coop exec") {
		t.Fatalf("plan_only launch did not emit coop exec: %s", stdout)
	}
	assertNoLaunchBinding(t, filepath.Join(project, defaultCoopRoot, "collab"))
}

func TestLaunchPathInspectUnknownMakesZeroMutations(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	sessionRoot := filepath.Join(project, defaultCoopRoot, "collab")
	creates, closes := 0, 0
	launchBackends = func() map[string]launch.Backend {
		return map[string]launch.Backend{
			launch.LauncherCommands: countingLaunchBackend{Backend: launch.Commands{}, creates: &creates, closes: &closes},
		}
	}
	writeCommandsBinding(t, sessionRoot)
	before := readBindingFile(t, sessionRoot)
	launchIsTerminal = func() bool { return true }
	launchInput = func() *bufio.Reader { return bufio.NewReader(strings.NewReader("y\n")) }

	_, _, trustErr := captureEnvOutput(t, func() error { return runLaunch([]string{}) })
	if GetExitCode(trustErr) != ExitActionRequired || !strings.Contains(trustErr.Error(), "inspect_unknown") {
		t.Fatalf("interactive inspect_unknown: exit=%d err=%v", GetExitCode(trustErr), trustErr)
	}
	stdout, _, err := captureEnvOutput(t, func() error { return runLaunch([]string{"--json"}) })
	if GetExitCode(err) != ExitActionRequired {
		t.Fatalf("exit=%d err=%v output=%s", GetExitCode(err), err, stdout)
	}
	var result launch.ReconcileResult
	if json.Unmarshal([]byte(stdout), &result) != nil || result.Reason != "inspect_unknown" {
		t.Fatalf("result=%s", stdout)
	}
	if creates != 0 || closes != 0 {
		t.Fatalf("Inspect unknown mutated backend: creates=%d closes=%d", creates, closes)
	}
	if after := readBindingFile(t, sessionRoot); after != before {
		t.Fatal("Inspect unknown mutated the seeded binding")
	}
}

func TestLaunchPathTypoRefusalUnknownSession(t *testing.T) {
	project, _ := launchCLIFixture(t, "collab")
	before := snapshotTree(t, project)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runSession([]string{"resume", "no-such-session"})
	})
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("exit=%d err=%v output=%s", GetExitCode(err), err, stdout)
	}
	after := snapshotTree(t, project)
	if strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("unknown resume mutated the tree\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

func TestWaveACloseEvidenceSetupLaunchResume(t *testing.T) {
	project := setupProjectFixture(t, "claude")
	clearPinnedSessionEnv(t)
	setupOut, err := captureEnvStdout(t, func() error {
		return runSetup([]string{
			"-y", "--agents", "claude", "--default-session", "collab",
			"--launcher-preference", "commands", "--no-gitignore", "--json",
		})
	})
	if err != nil {
		t.Fatalf("setup -y: %v\n%s", err, setupOut)
	}

	overlayLaunchEngine(t)

	untrustedOut, _, untrustedErr := captureEnvOutput(t, func() error { return runLaunch([]string{"--json"}) })
	if GetExitCode(untrustedErr) != ExitActionRequired {
		t.Fatalf("untrusted launch exit=%d err=%v output=%s", GetExitCode(untrustedErr), untrustedErr, untrustedOut)
	}
	if !strings.Contains(untrustedOut, "launch plan requires local trust confirmation") {
		t.Fatalf("untrusted output missing trust reason: %s", untrustedOut)
	}

	unknownOut, _, unknownErr := captureEnvOutput(t, func() error {
		return runSession([]string{"resume", "missing"})
	})
	if GetExitCode(unknownErr) != ExitNotFound {
		t.Fatalf("unknown resume exit=%d err=%v output=%s", GetExitCode(unknownErr), unknownErr, unknownOut)
	}

	launchIsTerminal = func() bool { return true }
	launchInput = func() *bufio.Reader { return bufio.NewReader(strings.NewReader("y\n")) }
	trustedOut, _, trustedErr := captureEnvOutput(t, func() error { return runLaunch([]string{}) })
	if GetExitCode(trustedErr) != ExitActionRequired {
		t.Fatalf("trusted launch exit=%d err=%v output=%s", GetExitCode(trustedErr), trustedErr, trustedOut)
	}
	if !strings.Contains(trustedOut, "coop exec") {
		t.Fatalf("trusted launch did not emit coop exec: %s", trustedOut)
	}

	t.Logf("evidence setup: %s", strings.TrimSpace(setupOut))
	t.Logf("evidence untrusted launch: %s", strings.TrimSpace(untrustedOut))
	t.Logf("evidence unknown resume: %v", unknownErr)
	t.Logf("evidence commands emission: %s", strings.TrimSpace(trustedOut))
	_ = project
}

func overlayLaunchEngine(t *testing.T) {
	t.Helper()
	state := t.TempDir()
	launchIsTerminal = func() bool { return false }
	launchStateDir = func() (string, error) { return state, nil }
	launchAMQPath = func() string { path, _ := os.Executable(); return path }
	launchAdapters = func(launch.ProjectConfig) map[string]launch.HarnessAdapter {
		return map[string]launch.HarnessAdapter{"claude": launchFixtureAdapter{available: true}}
	}
	launchBackends = func() map[string]launch.Backend {
		return map[string]launch.Backend{launch.LauncherCommands: launch.Commands{}}
	}
	launchHostname = func() (string, error) { return "host:test", nil }
	t.Cleanup(func() {
		launchIsTerminal = func() bool { return false }
		launchInput = func() *bufio.Reader { return bufio.NewReader(os.Stdin) }
		launchStateDir = defaultLaunchStateDir
		launchAMQPath = func() string { path, _ := os.Executable(); return path }
		launchAdapters = defaultLaunchAdapters
		launchBackends = func() map[string]launch.Backend {
			return map[string]launch.Backend{launch.LauncherCommands: launch.Commands{}}
		}
		launchHostname = os.Hostname
	})
}

func clearPinnedSessionEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{envRoot, envBaseRoot, envRootID, envBaseRootID, envSession, envGlobalRoot} {
		value, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func writeCommandsBinding(t *testing.T, sessionRoot string) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(sessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, err := launch.AcquireLease(root, "nonce-test")
	if err != nil {
		t.Fatal(err)
	}
	record := launch.BindingRecord{
		Version: launch.BindingVersion, Backend: launch.CommandsBackendName,
		HostIdentity: "host:test", InstanceIdentity: "commands:test",
		Profile: launch.CommandsProfile().Identity(), LaunchNonce: "nonce-test",
		Resources: launch.ResourceIdentitySet{Version: launch.ResourceSetVersion, Resources: []launch.ResourceIdentity{{OpaqueID: "resource:test"}}},
	}
	if err := launch.WriteBinding(root, lease, record); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func assertNoLaunchBinding(t *testing.T, sessionRoot string) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(sessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := launch.LoadBinding(root); err == nil {
		t.Fatal("plan_only launch wrote a binding")
	}
}

func readBindingFile(t *testing.T, sessionRoot string) string {
	t.Helper()
	data, err := os.ReadFile(launch.BindingPath(sessionRoot))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		names = append(names, rel+"\t"+info.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}
