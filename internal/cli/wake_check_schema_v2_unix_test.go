//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeCheckJSONSchemaV2DirectStartUsesExplicitNullsAndArgv(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{
			"check", "--root", root, "--me", "codex",
			"--json", "--json-schema=2",
		})
	})
	if err != nil {
		t.Fatalf("wake check v2: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode wake check v2: %v\n%s", err, output)
	}
	if got["schema"] != float64(2) || got["agent"] != "codex" || got["root"] != canonicalWakeRoot(root) {
		t.Fatalf("identity = %#v", got)
	}
	platform := requireJSONObject(t, got, "platform")
	if platform["wake_supported"] != true || platform["reason_code"] != nil {
		t.Fatalf("platform = %#v", platform)
	}
	start := requireJSONObject(t, got, "start")
	if start["available"] != true || start["mode"] != wakeInjectModeRaw ||
		start["reason_code"] != nil || start["detail"] != nil {
		t.Fatalf("start = %#v", start)
	}
	wake := requireJSONObject(t, got, "wake")
	if wake["status"] != string(wakeLockMissing) || wake["live"] != false ||
		wake["pid"] != nil || wake["mode"] != nil || wake["owner_bound"] != false ||
		wake["generation"] != nil || wake["target_digest"] != nil {
		t.Fatalf("wake = %#v", wake)
	}
	image := requireJSONObject(t, got, "image")
	running := requireJSONObject(t, image, "running")
	if running["path"] != nil || running["version"] != nil || image["status"] != wakeImageUnknown {
		t.Fatalf("image = %#v", image)
	}
	repair := requireJSONObject(t, got, "repair")
	if repair["inject_via_available"] != false || repair["reason_code"] != "no_wake_lock" || repair["detail"] != nil {
		t.Fatalf("repair = %#v", repair)
	}
	if got["restart_capability"] != wakeRestartAgentSafe {
		t.Fatalf("restart_capability = %#v", got["restart_capability"])
	}
	action := requireJSONObject(t, got, "action")
	if action["kind"] != "start_wake" || action["actor"] != "agent" ||
		action["reason_code"] != "wake_missing_start_available" || action["terminal_required"] != false {
		t.Fatalf("action = %#v", action)
	}
	command := requireJSONObject(t, action, "command")
	program, ok := command["program"].(string)
	if !ok || !filepath.IsAbs(program) {
		t.Fatalf("action command program = %#v", command["program"])
	}
	wantArgs := []any{"wake", "--root", canonicalWakeRoot(root), "--me", "codex"}
	if !reflect.DeepEqual(command["args"], wantArgs) {
		t.Fatalf("action command args = %#v, want %#v", command["args"], wantArgs)
	}
}

func TestWakeCheckV2AdvertisedStartRevalidatesChangedWakeState(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubWakeCheckRuntime(t, true, "0.50.1")
	decision := inspectWakeCheckDecision(root, "codex")
	if decision.Action.Kind != wakeActionStartWake ||
		decision.Action.Actor != wakeActionActorAgent ||
		decision.Action.Command == nil {
		t.Fatalf("advertised start = %#v", decision)
	}

	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		TTY:          "unknown",
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		ProcessStart: "12345",
		BootID:       "same-boot",
		Executable:   "/opt/homebrew/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "arrived-after-check",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID: pid, Running: true, StartToken: "12345", BootID: "same-boot",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex"},
		}
	})
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	loopCalled := false
	err = runWakeWithLoop(decision.Action.Command.Args[1:], func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	if err == nil || loopCalled {
		t.Fatalf("changed-state start: err=%v loop_called=%t", err, loopCalled)
	}
	after, readErr := os.ReadFile(lockPath)
	if readErr != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("changed-state start altered wake lock: err=%v before=%q after=%q", readErr, before, after)
	}
}

func requireJSONObject(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, object[key])
	}
	return value
}
