//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeRepairNotifiesMessageDeliveredDuringDowntime(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	writeWakeContinuityMessage(t, root, "startup.md", "startup", "startup backlog", "normal", nil)

	helper := copyTestBinaryForWakeContinuity(t)
	injectedPath := filepath.Join(root, "injected.txt")
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	injectorScript := "#!/bin/sh\noutput=$1\nshift\nprintf '%s\\n' \"$@\" >> \"$output\"\n"
	if err := os.WriteFile(injector, []byte(injectorScript), 0o700); err != nil {
		t.Fatalf("write injector: %v", err)
	}

	readyPath := filepath.Join(root, "initial.ready")
	initial := exec.Command(
		helper,
		"--no-update-check",
		"wake",
		"--root", root,
		"--me", "codex",
		"--baseline-existing",
		"--inject-via", injector,
		"--inject-arg", injectedPath,
		"--ready-file", readyPath,
	)
	initial.Env = wakeContinuityCLIEnv()
	if err := initial.Start(); err != nil {
		t.Fatalf("start initial wake: %v", err)
	}
	initialWaiter := newWakeProcessWaiter(initial.Process)
	t.Cleanup(func() {
		_ = initial.Process.Kill()
		_ = initialWaiter.waitForExit(time.Second)
	})
	if err := waitForWakeReadyWithWaiter(initialWaiter, readyPath, root, "codex", 5*time.Second); err != nil {
		t.Fatalf("wait for initial wake: %v\ninspection: %#v", err, inspectWakeLock(root, "codex"))
	}

	if err := initial.Process.Kill(); err != nil {
		t.Fatalf("kill initial wake: %v", err)
	}
	if err := initialWaiter.waitForExit(5 * time.Second); err != nil {
		t.Fatalf("wait for killed initial wake: %v", err)
	}

	writeWakeContinuityMessage(
		t,
		root,
		"downtime.md",
		"downtime",
		"urgent message delivered during downtime",
		"urgent",
		[]string{"interrupt"},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repair := exec.CommandContext(
		ctx,
		helper,
		"--no-update-check",
		"wake",
		"repair",
		"--root", root,
		"--me", "codex",
		"--json",
	)
	repair.Env = wakeContinuityCLIEnv()
	output, err := repair.CombinedOutput()
	if err != nil {
		t.Fatalf("repair wake: %v\noutput: %s", err, output)
	}
	var result wakeRepairResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode repair result: %v\noutput: %s", err, output)
	}
	if result.Status != "repaired" {
		t.Fatalf("repair result = %#v", result)
	}
	t.Cleanup(func() {
		retire := exec.Command(
			helper,
			"--no-update-check",
			"wake",
			"retire",
			"--root", root,
			"--me", "codex",
			"--inject-via", injector,
			"--inject-arg", injectedPath,
		)
		retire.Env = wakeContinuityCLIEnv()
		retireOutput, retireErr := retire.CombinedOutput()
		if stopped := stopWakeContinuityHelper(result.PID, helper, root, "codex"); !stopped && retireErr != nil {
			t.Logf("stop repaired wake: retire=%v output=%s", retireErr, retireOutput)
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(injectedPath)
		if readErr == nil && strings.Contains(string(data), "urgent message delivered during downtime") {
			if strings.Contains(string(data), "startup backlog") {
				t.Fatalf("repaired wake re-notified the startup backlog: %q", data)
			}
			return
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatalf("read injected output: %v", readErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, _ := os.ReadFile(injectedPath)
	t.Fatalf("repaired wake suppressed the downtime message; injected output: %q", data)
}

func stopWakeContinuityHelper(pid int, helper, root, me string) bool {
	proc := inspectWakeProcessPlatform(pid)
	if !proc.Running {
		return true
	}
	if filepath.Clean(proc.Executable) != filepath.Clean(helper) ||
		!wakeArgsMatchRootAgent(proc.Args, root, me) {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !processAlive(pid)
}

func copyTestBinaryForWakeContinuity(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	path := filepath.Join(secureTempDirForTest(t), "amq")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatalf("write test helper: %v", err)
	}
	return path
}

func wakeContinuityCLIEnv() []string {
	drop := []string{
		"AM_ROOT=",
		"AM_ROOT_ID=",
		"AM_BASE_ROOT=",
		"AM_BASE_ROOT_ID=",
		"AM_SESSION=",
		"AM_ME=",
		"AMQ_GLOBAL_ROOT=",
		"AMQ_WAKE_OWNER=",
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		keep := true
		for _, prefix := range drop {
			if strings.HasPrefix(value, prefix) {
				keep = false
				break
			}
		}
		if keep {
			env = append(env, value)
		}
	}
	return append(env, cliHelperEnv+"=1", "AMQ_NO_UPDATE_CHECK=1")
}

func writeWakeContinuityMessage(
	t *testing.T,
	root string,
	filename string,
	id string,
	subject string,
	priority string,
	labels []string,
) {
	t.Helper()
	msg := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       id,
			From:     "claude",
			To:       []string{"codex"},
			Thread:   "p2p/claude__codex",
			Subject:  subject,
			Created:  "2026-07-24T00:00:00Z",
			Priority: priority,
			Labels:   labels,
		},
		Body: "body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal %s: %v", id, err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", filename, data); err != nil {
		t.Fatalf("deliver %s: %v", id, err)
	}
}
