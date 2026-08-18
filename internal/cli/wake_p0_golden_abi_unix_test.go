//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	wakeABIRootEnv       = "AM_ROOT"
	wakeABIRootIDEnv     = "AM_ROOT_ID"
	wakeABIBaseRootEnv   = "AM_BASE_ROOT"
	wakeABIBaseRootIDEnv = "AM_BASE_ROOT_ID"
	wakeABISessionEnv    = "AM_SESSION"
	wakeABIGlobalRootEnv = "AMQ_GLOBAL_ROOT"
	wakeABIWakeOwnerEnv  = "AMQ_WAKE_OWNER"
	wakeABITIOCSTIPath   = "/proc/sys/dev/tty/legacy_tiocsti"
)

// These tests deliberately invoke a freshly built cmd/amq binary. They are
// the process-level ABI floor for wake diagnostics and state, rather than
// another set of in-process tests that could accidentally share implementation
// details with the code under test.

type wakeABIRun struct {
	stdout []byte
	stderr []byte
	code   int
}

func buildWakeABIBinary(t *testing.T) (string, string) {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve ABI test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))

	binary := filepath.Join(t.TempDir(), "amq")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/amq")
	cmd.Dir = repoRoot
	cmd.Env = wakeABICleanEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build amq timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("build amq: %v\n%s", err, output)
	}
	return binary, repoRoot
}

func wakeABICleanEnv(extra ...string) []string {
	env := os.Environ()
	for _, name := range []string{
		wakeABIRootEnv,
		wakeABIRootIDEnv,
		wakeABIBaseRootEnv,
		wakeABIBaseRootIDEnv,
		wakeABISessionEnv,
		wakeABIGlobalRootEnv,
		wakeABIWakeOwnerEnv,
		"AMQ_WAKE_PRIVATE_STOP_FD",
	} {
		env = wakeABIUnsetEnv(env, name)
	}
	env = append(env, "AMQ_NO_UPDATE_CHECK=1")
	return append(env, extra...)
}

func wakeABIUnsetEnv(env []string, name string) []string {
	prefix := name + "="
	filtered := env[:0]
	for _, value := range env {
		if !strings.HasPrefix(value, prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func runWakeABIBinary(t *testing.T, binary, repoRoot string, env []string, args ...string) wakeABIRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = repoRoot
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
	}
	if ctx.Err() != nil {
		return wakeABIRun{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: -1}
	}
	return wakeABIRun{stdout: stdout.Bytes(), stderr: stderr.Bytes(), code: code}
}

func initializeWakeABIRoot(t *testing.T, binary, repoRoot, root string) {
	t.Helper()
	run := runWakeABIBinary(t, binary, repoRoot, wakeABICleanEnv(), "init", "--root", root, "--agents", "codex")
	if run.code != 0 {
		t.Fatalf("init failed (exit %d):\nstdout=%s\nstderr=%s", run.code, run.stdout, run.stderr)
	}
}

func canonicalWakeABIRoot(t *testing.T, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root %q: %v", root, err)
	}
	return filepath.Clean(resolved)
}

func canonicalWakeABIBinary(t *testing.T, binary string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatalf("resolve binary %q: %v", binary, err)
	}
	return filepath.Clean(resolved)
}

func wakeABIMissingLockStart(t *testing.T) (mode, reason, detail string) {
	t.Helper()
	mode = "raw"
	reason = "owning_terminal_required"
	detail = "stdin is not a TTY; start the wake from its owning terminal"
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile(wakeABITIOCSTIPath); err == nil && strings.TrimSpace(string(data)) == "0" {
			mode = "none"
			reason = "full_strength_injector_unavailable"
			detail = "TIOCSTI is disabled; a full-strength terminal wake cannot start here"
		}
	}
	return mode, reason, detail
}

func wakeABIShellQuoteArg(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case strings.ContainsRune("@%_+=:,./-", r):
			return false
		default:
			return true
		}
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func wakeABIWakeReasonCodes() map[string]bool {
	return map[string]bool{
		"wake_missing_start_available":        true,
		"full_strength_injector_unavailable":  true,
		"owning_terminal_required":            true,
		"stale_inject_via_repair_available":   true,
		"live_wake_preserve":                  true,
		"owner_bound_stale_recovery_required": true,
		"stale_lock_manual_cleanup_required":  true,
		"binary_dir_gone":                     true,
		"wake_state_creating":                 true,
		"wake_state_unverified":               true,
		"observation_changed":                 true,
		"executable_identity_unavailable":     true,
		"platform_unsupported":                true,
		"reload_ready":                        true,
		"reload_not_prepared":                 true,
		"reload_restart_pending":              true,
	}
}

func wakeABIRepairReasonCodes() map[string]bool {
	return map[string]bool{
		"no_wake_lock":                      true,
		"owner_bound":                       true,
		"wake_live":                         true,
		"wake_not_stale":                    true,
		"exact_repair_evidence_unavailable": true,
	}
}

func wakeABIReloadReasonCodes() map[string]bool {
	return map[string]bool{
		"reload_not_live":              true,
		"reload_not_advertised":        true,
		"reload_schema_unsupported":    true,
		"reload_advertisement_invalid": true,
		"reload_observation_changed":   true,
		"reload_platform_unsupported":  true,
		"reload_command_unavailable":   true,
		"reload_ready":                 true,
		"reload_owner_mismatch":        true,
		"reload_not_prepared":          true,
		"reload_restart_pending":       true,
	}
}

func TestWakeP0BinaryV1DefaultCheckSchemaBytes(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	root := t.TempDir()
	initializeWakeABIRoot(t, binary, repoRoot, root)
	version := wakeABIBinaryVersion(t, binary, repoRoot)

	defaultRun := runWakeABIBinary(t, binary, repoRoot, wakeABICleanEnv(),
		"wake", "check", "--root", root, "--me", "codex", "--json")
	explicitRun := runWakeABIBinary(t, binary, repoRoot, wakeABICleanEnv(),
		"wake", "check", "--root", root, "--me", "codex", "--json", "--json-schema=1")
	for name, run := range map[string]wakeABIRun{"default": defaultRun, "explicit schema 1": explicitRun} {
		if run.code != 0 {
			t.Fatalf("%s wake check failed (exit %d):\nstdout=%s\nstderr=%s", name, run.code, run.stdout, run.stderr)
		}
		if len(run.stderr) != 0 {
			t.Fatalf("%s wake check wrote stderr on successful JSON output: %q", name, run.stderr)
		}
		var decoded map[string]any
		if err := json.Unmarshal(run.stdout, &decoded); err != nil {
			t.Fatalf("%s wake check JSON: %v\n%s", name, err, run.stdout)
		}
		if decoded["schema"] != float64(1) || decoded["agent"] != "codex" {
			t.Fatalf("%s wake check identity = %#v", name, decoded)
		}
	}

	// Schema 1 is the compatibility default. This is the promised byte-level
	// invariant: selecting it explicitly must not alter the default stream.
	if !bytes.Equal(defaultRun.stdout, explicitRun.stdout) {
		t.Fatalf("wake check schema-1 default bytes changed:\ndefault:\n%s\nexplicit:\n%s", defaultRun.stdout, explicitRun.stdout)
	}

	want := wakeABIV1MissingLockGolden(t, root, binary, version)
	if !bytes.Equal(defaultRun.stdout, want) {
		t.Fatalf("wake check schema-1 golden bytes changed:\n--- got ---\n%s--- want ---\n%s", defaultRun.stdout, want)
	}
}

func TestWakeP0BinaryDiagnosticsIgnoreP2aStatePresenceAndValidity(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	root := t.TempDir()
	initializeWakeABIRoot(t, binary, repoRoot, root)
	injector := writeExecutableForTest(t, "wake-p0-dual-read-injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"p2a"})
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}

	absent := runWakeP0StateDiagnostics(t, binary, repoRoot, root)
	publishWakeStateForDualReadTest(t, fixture)
	present := runWakeP0StateDiagnostics(t, binary, repoRoot, root)
	assertWakeP0StateDiagnosticsEqual(t, absent, present, "eligible state")
	statePath := filepath.Join(agentDir.path, wakeStateFileName)
	if err := os.WriteFile(statePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformed := runWakeP0StateDiagnostics(t, binary, repoRoot, root)
	assertWakeP0StateDiagnosticsEqual(t, absent, malformed, "malformed state")
}

func TestWakeP0BinaryDiagnosticsPreserveStableInvalidPreparedEvidence(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "mode-0640",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := path + ".symlink-target"
				if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxWakeMetadataFileBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			initializeWakeABIRoot(t, binary, repoRoot, root)
			injector := writeExecutableForTest(t, "wake-p0-invalid-prepared-injector")
			target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"p2a"})
			if err := writeWakeTarget(root, "codex", target); err != nil {
				t.Fatal(err)
			}
			digest, err := wakeTargetDigest(target)
			if err != nil {
				t.Fatal(err)
			}
			preparedPath := wakePreparedPath(root, "codex")
			if err := writeWakeGenerationFile(preparedPath, "wake prepared marker", wakeReady{
				Schema:       wakeReadySchema,
				Generation:   "11111111111111111111111111111111",
				TargetDigest: digest,
			}); err != nil {
				t.Fatal(err)
			}
			agentDir, err := openWakeAgentDir(root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentDir.Close() })
			fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}
			publishWakeStateForDualReadTest(t, fixture)
			statePath := filepath.Join(agentDir.path, wakeStateFileName)
			stateRaw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(statePath); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, preparedPath)

			legacy := runWakeP0StateDiagnostics(t, binary, repoRoot, root)
			assertWakeP0DiagnosticsHaveNoFalseSnapshotChange(t, legacy, "legacy-only")
			if err := os.WriteFile(statePath, stateRaw, 0o600); err != nil {
				t.Fatal(err)
			}
			projected := runWakeP0StateDiagnostics(t, binary, repoRoot, root)
			assertWakeP0StateDiagnosticsEqual(t, legacy, projected, "stable invalid prepared")
			assertWakeP0DiagnosticsHaveNoFalseSnapshotChange(t, projected, "state present")
		})
	}
}

func assertWakeP0DiagnosticsHaveNoFalseSnapshotChange(
	t *testing.T,
	runs [2]wakeABIRun,
	label string,
) {
	t.Helper()
	for index, name := range []string{"wake check", "doctor ops"} {
		combined := append(bytes.Clone(runs[index].stdout), runs[index].stderr...)
		if bytes.Contains(combined, []byte("wake legacy state changed")) ||
			bytes.Contains(combined, []byte("observation changed")) {
			t.Fatalf("%s %s falsely reported a stable snapshot change: %s", label, name, combined)
		}
	}
}

func runWakeP0StateDiagnostics(
	t *testing.T,
	binary string,
	repoRoot string,
	root string,
) [2]wakeABIRun {
	t.Helper()
	env := wakeABICleanEnv()
	return [2]wakeABIRun{
		runWakeABIBinary(t, binary, repoRoot, env,
			"wake", "check", "--root", root, "--me", "codex", "--json", "--json-schema=2"),
		runWakeABIBinary(t, binary, repoRoot, env,
			"doctor", "--root", root, "--ops", "--json", "--json-schema=2"),
	}
}

func assertWakeP0StateDiagnosticsEqual(
	t *testing.T,
	want [2]wakeABIRun,
	got [2]wakeABIRun,
	label string,
) {
	t.Helper()
	for index, name := range []string{"wake check", "doctor ops"} {
		if want[index].code != got[index].code ||
			!bytes.Equal(want[index].stdout, got[index].stdout) ||
			!bytes.Equal(want[index].stderr, got[index].stderr) {
			t.Fatalf(
				"%s changed %s process bytes:\nwant code=%d stdout=%s stderr=%s\ngot code=%d stdout=%s stderr=%s",
				label,
				name,
				want[index].code,
				want[index].stdout,
				want[index].stderr,
				got[index].code,
				got[index].stdout,
				got[index].stderr,
			)
		}
	}
}

func wakeABIBinaryVersion(t *testing.T, binary, repoRoot string) string {
	t.Helper()
	run := runWakeABIBinary(t, binary, repoRoot, wakeABICleanEnv(), "--version")
	if run.code != 0 || len(run.stderr) != 0 {
		t.Fatalf("amq --version failed (exit %d): stdout=%q stderr=%q", run.code, run.stdout, run.stderr)
	}
	version := strings.TrimSpace(string(run.stdout))
	if version == "" {
		t.Fatal("amq --version returned an empty version")
	}
	return version
}

func wakeABIV1MissingLockGolden(t *testing.T, root, binary, version string) []byte {
	t.Helper()
	root = canonicalWakeABIRoot(t, root)
	image := canonicalWakeABIBinary(t, binary)
	quote := func(value string) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("quote golden value: %v", err)
		}
		return string(encoded)
	}

	startMode, _, startReason := wakeABIMissingLockStart(t)
	restartCapability := "operator_only"
	operatorTerminalRequired := true
	var nextAction string
	if startMode == "none" {
		restartCapability = "unavailable"
		operatorTerminalRequired = false
		nextAction = "restore a supported full-strength injector or configure --inject-via; do not accept an attention-only downgrade"
	} else {
		nextAction = "from the owning terminal, run amq wake --root " + wakeABIShellQuoteArg(root) + " --me codex"
	}

	golden := fmt.Sprintf(
		"{\n"+
			"  \"schema\": 1,\n"+
			"  \"agent\": \"codex\",\n"+
			"  \"root\": %s,\n"+
			"  \"can_start_here\": false,\n"+
			"  \"start_mode\": %s,\n"+
			"  \"start_reason\": %s,\n"+
			"  \"live_wake\": false,\n"+
			"  \"wake_status\": \"missing\",\n"+
			"  \"owner_bound\": false,\n"+
			"  \"running_image_path\": \"unknown\",\n"+
			"  \"running_version\": \"unknown\",\n"+"  \"current_image_path\": %s,\n"+
			"  \"current_version\": %s,\n"+
			"  \"image_status\": \"unknown\",\n"+
			"  \"can_repair_inject_via\": false,\n"+
			"  \"repair_reason\": \"no wake lock is present\",\n"+
			"  \"restart_capability\": %s,\n"+
			"  \"operator_terminal_required\": %t,\n"+
			"  \"next_action\": %s\n"+
			"}\n",
		quote(root), quote(startMode), quote(startReason), quote(image), quote(version),
		quote(restartCapability), operatorTerminalRequired, quote(nextAction),
	)
	return []byte(golden)
}

func TestWakeP0BinaryJSONFieldsAndEnums(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	root := t.TempDir()
	initializeWakeABIRoot(t, binary, repoRoot, root)

	run := runWakeABIBinary(t, binary, repoRoot, wakeABICleanEnv(),
		"wake", "check", "--root", root, "--me", "codex", "--json", "--json-schema=2")
	if run.code != 0 || len(run.stderr) != 0 {
		t.Fatalf("wake check v2 process streams = exit %d, stdout=%s, stderr=%s", run.code, run.stdout, run.stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(run.stdout, &result); err != nil {
		t.Fatalf("decode wake check v2: %v\n%s", err, run.stdout)
	}
	if result["schema"] != float64(2) || result["agent"] != "codex" {
		t.Fatalf("wake check v2 identity = %#v", result)
	}

	platform := wakeABINestedObject(t, result, "platform")
	start := wakeABINestedObject(t, result, "start")
	wake := wakeABINestedObject(t, result, "wake")
	image := wakeABINestedObject(t, result, "image")
	repair := wakeABINestedObject(t, result, "repair")
	reload := wakeABINestedObject(t, result, "reload")
	action := wakeABINestedObject(t, result, "action")

	if platform["os"] != runtime.GOOS || platform["wake_supported"] != true {
		t.Fatalf("platform contract = %#v", platform)
	}
	wakeABIAssertEnum(t, "start.mode", start["mode"], map[string]bool{"raw": true, "paste": true, "none": true})
	wakeABIAssertEnum(t, "wake.status", wake["status"], map[string]bool{
		"missing": true, "valid": true, "stale": true, "creating": true, "unverified": true,
	})
	wakeABIAssertEnum(t, "image.status", image["status"], map[string]bool{"current": true, "different": true, "unknown": true})
	wakeABIAssertEnum(t, "reload.status", reload["status"], map[string]bool{"advertised": true, "ready": true, "unavailable": true})
	wakeABIAssertEnum(t, "restart_capability", result["restart_capability"], map[string]bool{
		"agent_safe": true, "operator_only": true, "unavailable": true,
	})
	wakeABIAssertEnum(t, "action.kind", action["kind"], map[string]bool{
		"start_wake": true, "repair_wake": true, "restart_wake": true, "recover_owner": true,
		"preserve_live_wake": true, "inspect_unverified": true, "retry_check": true,
		"configure_injector": true, "manual_stale_cleanup": true,
		"wait_for_stable_state": true, "unsupported": true,
	})
	wakeABIAssertEnum(t, "action.actor", action["actor"], map[string]bool{"agent": true, "operator": true, "none": true})
	wakeABIAssertEnum(t, "action.reason_code", action["reason_code"], wakeABIWakeReasonCodes())
	wakeABIAssertEnum(t, "reload.reason_code", reload["reason_code"], wakeABIReloadReasonCodes())
	wakeABIAssertEnum(t, "repair.reason_code", repair["reason_code"], wakeABIRepairReasonCodes())
	if platform["reason_code"] != nil {
		t.Fatalf("platform.reason_code = %#v, want null", platform["reason_code"])
	}
	startMode, startReason, startDetail := wakeABIMissingLockStart(t)
	if start["mode"] != startMode || start["reason_code"] != startReason || start["detail"] != startDetail {
		t.Fatalf("start missing-lock decision = %#v, want mode=%q reason=%q detail=%q", start, startMode, startReason, startDetail)
	}
	if wake["status"] != "missing" || wake["live"] != false || wake["owner_bound"] != false || wake["pid"] != nil || wake["mode"] != nil {
		t.Fatalf("wake missing-lock decision = %#v", wake)
	}
	if image["status"] != "unknown" {
		t.Fatalf("image.status = %#v, want unknown", image["status"])
	}
	runningImage := wakeABINestedObject(t, image, "running")
	if runningImage["path"] != nil || runningImage["version"] != nil {
		t.Fatalf("image.running = %#v, want explicit null evidence", runningImage)
	}
	if repair["reason_code"] != "no_wake_lock" || repair["detail"] != nil {
		t.Fatalf("repair missing-lock decision = %#v", repair)
	}
	if reload["status"] != "unavailable" || reload["reason_code"] != "reload_not_live" {
		t.Fatalf("reload missing-lock decision = %#v", reload)
	}
	if startMode == "none" {
		if result["restart_capability"] != "unavailable" || action["kind"] != "configure_injector" || action["actor"] != "operator" || action["reason_code"] != "full_strength_injector_unavailable" || action["terminal_required"] != false || action["command"] != nil {
			t.Fatalf("no-injector action = %#v, result=%#v", action, result)
		}
	} else {
		if result["restart_capability"] != "operator_only" || action["kind"] != "start_wake" || action["actor"] != "operator" || action["reason_code"] != "owning_terminal_required" || action["terminal_required"] != true {
			t.Fatalf("owning-terminal action = %#v, result=%#v", action, result)
		}
		command := wakeABINestedObject(t, action, "command")
		if command["program"] != filepath.Clean(binary) {
			t.Fatalf("action.command.program = %#v", command["program"])
		}
		args, ok := command["args"].([]any)
		if !ok || len(args) != 5 || args[0] != "wake" || args[1] != "--root" || args[2] != canonicalWakeABIRoot(t, root) || args[3] != "--me" || args[4] != "codex" {
			t.Fatalf("action.command.args = %#v", command["args"])
		}
	}
}

func TestWakeP0BinaryInspectionIsReadOnlyAndJSONStaysOnStdout(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	root := t.TempDir()
	initializeWakeABIRoot(t, binary, repoRoot, root)

	before, err := snapshotWakeABIRoot(root)
	if err != nil {
		t.Fatalf("snapshot before inspection: %v", err)
	}
	commands := [][]string{
		{"wake", "check", "--root", root, "--me", "codex", "--json"},
		{"doctor", "--root", root, "--ops", "--json"},
	}
	for _, args := range commands {
		run := runWakeABIBinary(t, binary, repoRoot, wakeABICleanEnv(), args...)
		if run.code != 0 {
			t.Fatalf("inspection %v exited %d: stdout=%s stderr=%s", args, run.code, run.stdout, run.stderr)
		}
		if len(run.stdout) == 0 || !json.Valid(run.stdout) {
			t.Fatalf("inspection %v did not emit one JSON document on stdout: %q", args, run.stdout)
		}
		if len(run.stderr) != 0 {
			t.Fatalf("inspection %v leaked diagnostics onto stderr: %q", args, run.stderr)
		}
		after, err := snapshotWakeABIRoot(root)
		if err != nil {
			t.Fatalf("snapshot after %v: %v", args, err)
		}
		if diff := diffWakeABISnapshots(before, after); diff != "" {
			t.Fatalf("inspection %v mutated the queue:\n%s", args, diff)
		}
	}
}

func TestWakeP0BinaryContextMismatchExitCodeFive(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	pinnedRoot := t.TempDir()
	targetRoot := t.TempDir()
	initializeWakeABIRoot(t, binary, repoRoot, pinnedRoot)
	initializeWakeABIRoot(t, binary, repoRoot, targetRoot)
	before, err := snapshotWakeABIRoot(targetRoot)
	if err != nil {
		t.Fatal(err)
	}

	env := wakeABICleanEnv(
		wakeABIRootEnv+"="+pinnedRoot,
		wakeABIBaseRootEnv+"="+pinnedRoot,
		wakeABISessionEnv+"=",
	)
	run := runWakeABIBinary(t, binary, repoRoot, env,
		"drain", "--root", targetRoot, "--me", "codex", "--json")
	if run.code != 5 {
		t.Fatalf("context mismatch exit = %d, want 5; stdout=%s stderr=%s", run.code, run.stdout, run.stderr)
	}
	if len(run.stdout) != 0 || !strings.Contains(string(run.stderr), "session context mismatch") {
		t.Fatalf("context mismatch streams: stdout=%q stderr=%q", run.stdout, run.stderr)
	}
	after, err := snapshotWakeABIRoot(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if diff := diffWakeABISnapshots(before, after); diff != "" {
		t.Fatalf("refused drain mutated target root:\n%s", diff)
	}
}

func TestWakeP0BinaryWakeLockJSONShape(t *testing.T) {
	binary, repoRoot := buildWakeABIBinary(t)
	root := t.TempDir()
	initializeWakeABIRoot(t, binary, repoRoot, root)
	ready := filepath.Join(t.TempDir(), "wake-ready")

	cmd := exec.Command(binary,
		"wake", "--root", root, "--me", "codex",
		"--inject-mode", "none", "--interrupt=false", "--ready-file", ready,
	)
	cmd.Dir = repoRoot
	cmd.Env = wakeABICleanEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wake: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			stopWakeABIBinary(t, cmd, done)
		}
	}()
	if waitErr, exited := waitForWakeABIPath(ready, done); exited {
		stopped = true
		t.Fatalf("wake exited before readiness: %v\nstdout=%s\nstderr=%s", waitErr, stdout.Bytes(), stderr.Bytes())
	} else if waitErr != nil {
		t.Fatalf("wait for wake readiness: %v", waitErr)
	}

	lockPath := filepath.Join(root, "agents", "codex", ".wake.lock")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read wake lock: %v", err)
	}
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := lockInfo.Mode().Perm(); mode != 0o600 {
		t.Fatalf("wake lock mode = %o, want 0600 for a non-owner wake", mode)
	}
	var lock map[string]any
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatalf("decode wake lock JSON: %v\n%s", err, lockBytes)
	}
	expectedKeys := map[string]bool{
		"pid": true, "tty": true, "root": true, "agent": true, "hostname": true,
		"started": true, "process_start": true, "boot_id": true, "executable": true,
		"image_path": true, "image_version": true, "wake_mode": true, "generation": true,
	}
	// machine_id is best-effort: recorded when a stable machine identity is
	// available, omitted otherwise. Both are valid lock ABI states.
	if _, ok := lock["machine_id"]; ok {
		expectedKeys["machine_id"] = true
	}
	if runtime.GOOS == "linux" {
		expectedKeys["args"] = true
	} else {
		expectedKeys["running_image_evidence"] = true
	}
	wakeABIAssertExactKeys(t, lock, expectedKeys, "wake lock")
	if got := wakeABINumber(t, lock, "pid"); got <= 0 {
		t.Fatalf("wake lock pid = %v", got)
	}
	if got := wakeABIString(t, lock, "root"); got != canonicalWakeABIRoot(t, root) {
		t.Fatalf("wake lock root = %q", got)
	}
	if got := wakeABIString(t, lock, "agent"); got != "codex" {
		t.Fatalf("wake lock agent = %q", got)
	}
	for _, key := range []string{"tty", "started", "generation", "wake_mode"} {
		if strings.TrimSpace(wakeABIString(t, lock, key)) == "" {
			t.Fatalf("wake lock %s is empty: %#v", key, lock)
		}
	}
	if got := wakeABIString(t, lock, "wake_mode"); got != "none" {
		t.Fatalf("wake lock wake_mode = %q, want none", got)
	}
	for _, key := range []string{"hostname", "process_start", "boot_id", "executable", "image_path", "image_version"} {
		if strings.TrimSpace(wakeABIString(t, lock, key)) == "" {
			t.Fatalf("wake lock %s is empty: %#v", key, lock)
		}
	}
	if _, ok := lock["machine_id"]; ok {
		if strings.TrimSpace(wakeABIString(t, lock, "machine_id")) == "" {
			t.Fatalf("wake lock machine_id present but empty: %#v", lock)
		}
	}
	if runtime.GOOS == "linux" {
		args, ok := lock["args"].([]any)
		if !ok || len(args) == 0 {
			t.Fatalf("wake lock args = %#v, want non-empty array", lock["args"])
		}
	}
	if runtime.GOOS == "darwin" {
		obj, ok := lock["running_image_evidence"].(map[string]any)
		if !ok {
			t.Fatalf("running_image_evidence = %#v, want object", lock["running_image_evidence"])
		}
		wakeABIAssertExactKeys(t, obj, map[string]bool{
			"schema": true, "platform": true, "method": true, "execution_path": true,
			"device": true, "inode": true, "size": true, "ctime_ns": true,
			"sha256": true, "embedded_version": true,
		}, "running image evidence")
		if wakeABINumber(t, obj, "schema") != 1 {
			t.Fatalf("running_image_evidence.schema = %#v", obj["schema"])
		}
		if got := wakeABIString(t, obj, "platform"); got != runtime.GOOS {
			t.Fatalf("running_image_evidence.platform = %q", got)
		}
		for _, key := range []string{"method", "execution_path", "sha256", "embedded_version"} {
			if strings.TrimSpace(wakeABIString(t, obj, key)) == "" {
				t.Fatalf("running_image_evidence.%s is empty: %#v", key, obj)
			}
		}
		for _, key := range []string{"device", "inode", "size", "ctime_ns"} {
			if wakeABINumber(t, obj, key) <= 0 {
				t.Fatalf("running_image_evidence.%s = %#v", key, obj[key])
			}
		}
	}

	stopWakeABIBinary(t, cmd, done)
	stopped = true
}

func wakeABINestedObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, parent[key])
	}
	return value
}

func wakeABIAssertEnum(t *testing.T, name string, value any, allowed map[string]bool) {
	t.Helper()
	got, ok := value.(string)
	if !ok || !allowed[got] {
		t.Fatalf("%s = %#v, allowed=%v", name, value, sortedWakeABIKeys(allowed))
	}
}

func wakeABIAssertExactKeys(t *testing.T, object map[string]any, expected map[string]bool, name string) {
	t.Helper()
	actual := make(map[string]bool, len(object))
	for key := range object {
		actual[key] = true
	}
	if len(actual) != len(expected) {
		t.Fatalf("%s keys = %v, want %v", name, sortedWakeABIKeys(actual), sortedWakeABIKeys(expected))
	}
	for key := range expected {
		if !actual[key] {
			t.Fatalf("%s keys = %v, want %v", name, sortedWakeABIKeys(actual), sortedWakeABIKeys(expected))
		}
	}
}

func sortedWakeABIKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func wakeABIString(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want string", key, object[key])
	}
	return value
}

func wakeABINumber(t *testing.T, object map[string]any, key string) float64 {
	t.Helper()
	value, ok := object[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want number", key, object[key])
	}
	return value
}

type wakeABISnapshot map[string]string

func snapshotWakeABIRoot(root string) (wakeABISnapshot, error) {
	snapshot := make(wakeABISnapshot)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			snapshot[rel] = fmt.Sprintf("dir:%o", info.Mode().Perm())
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[rel] = fmt.Sprintf("symlink:%o:%s", info.Mode().Perm(), target)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[rel] = fmt.Sprintf("file:%o:%s", info.Mode().Perm(), string(data))
		return nil
	})
	return snapshot, err
}

func diffWakeABISnapshots(before, after wakeABISnapshot) string {
	keys := make(map[string]bool, len(before)+len(after))
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var differences []string
	for _, key := range ordered {
		if before[key] != after[key] {
			differences = append(differences, fmt.Sprintf("%s: before=%q after=%q", key, before[key], after[key]))
		}
	}
	return strings.Join(differences, "\n")
}

func waitForWakeABIPath(path string, done <-chan error) (error, bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return err, true
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return nil, false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("wake did not publish readiness file %s", path), false
}

func stopWakeABIBinary(t *testing.T, cmd *exec.Cmd, done <-chan error) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("wake did not exit after SIGTERM/SIGKILL")
		}
	}
}
