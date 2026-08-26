package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testQueueUUID = "01a01f5f-69d6-7dd0-868f-9376f3d2c0a1"

type stubWriterLock struct {
	held bool
	err  error
}

func (s stubWriterLock) Held(context.Context, string) (bool, error) {
	return s.held, s.err
}

func writeCodexQueueFixture(t *testing.T, uuid string) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "08", "26")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-sess-"+uuid+".jsonl")
	if err := os.WriteFile(rollout, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home, bin
}

func testCodexQueue(home, bin string, runner CommandRunner, lock WriterLockInspector) CodexQueue {
	return CodexQueue{
		Runner:            runner,
		LookPath:          func(string) (string, error) { return bin, nil },
		InspectWriterLock: lock,
		Home:              home,
	}
}

func TestDefaultRegistryRegistersCodexQueue(t *testing.T) {
	if _, err := DefaultRegistry().Get("codex-queue"); err != nil {
		t.Fatalf("DefaultRegistry().Get(%q) failed: %v", "codex-queue", err)
	}
	cap := (CodexQueue{}).Capability()
	if cap.Activation != ActivationNone || cap.Delivery != DeliverySubmitted || cap.Session != SessionExistingExact || cap.RequiresHuman {
		t.Fatalf("codex-queue capability = %+v, want none+submitted+existing-exact+unattended", cap)
	}
	if !cap.Satisfies(Capability{Delivery: DeliverySubmitted, Session: SessionExistingExact}) {
		t.Fatal("codex-queue does not satisfy submitted+existing-exact unattended min")
	}
}

func TestCodexQueueNormalizeTargetAcceptsThreadUUID(t *testing.T) {
	got, err := (CodexQueue{}).NormalizeTarget(" " + codexQueueTargetThreadPrefix + testQueueUUID + " ")
	if err != nil || got != codexQueueTargetThreadPrefix+testQueueUUID {
		t.Fatalf("NormalizeTarget() = %q, %v", got, err)
	}
}

func TestCodexQueueTargetRejectsUnsafeOrMalformedIdentity(t *testing.T) {
	for _, target := range []string{
		"",
		"codex-queue:new",
		"codex-queue:thread:",
		"codex-queue:thread:01A01F5F-69D6-7DD0-868F-9376F3D2C0A1",
		"codex-queue:thread:01a01f5f-69d6-7dd0-868f-9376f3d2c0a",
		"codex-queue:thread:../../../etc/passwd",
		"codex-queue:thread:01a01f5f-69d6-7dd0-868f-9376f3d2c0a1/extra",
		"codex-app:thread:" + testQueueUUID,
	} {
		if _, err := (CodexQueue{}).NormalizeTarget(target); err == nil {
			t.Fatalf("NormalizeTarget(%q) succeeded; want refusal", target)
		}
		if _, err := (CodexQueue{}).CapabilityForTarget(target); err == nil {
			t.Fatalf("CapabilityForTarget(%q) succeeded; want refusal", target)
		}
	}
}

func TestCodexQueueInjectUsesArgvOnlyAndPreservesPayload(t *testing.T) {
	home, bin := writeCodexQueueFixture(t, testQueueUUID)
	payload := "hello & ; `quotes`\nnewline"
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: []byte("Usage: codex queue --thread <THREAD> --message <TEXT>\n")},
		{},
	}}
	q := testCodexQueue(home, bin, runner, stubWriterLock{held: true})
	if err := q.Inject(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID, payload); err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d (%#v), want help + queue", len(runner.calls), runner.calls)
	}
	help := runner.calls[0]
	if help.name != bin || len(help.args) != 2 || help.args[0] != "queue" || help.args[1] != "--help" {
		t.Fatalf("probe help call = %#v", help)
	}
	enqueue := runner.calls[1]
	if enqueue.name != bin {
		t.Fatalf("enqueue executable = %q, want %q", enqueue.name, bin)
	}
	wantArgs := []string{"queue", "--thread", testQueueUUID, "--message", payload}
	if strings.Join(enqueue.args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("enqueue args = %#v, want %#v", enqueue.args, wantArgs)
	}
}

func TestCodexQueueIdleLockRefusesWithoutQueueCall(t *testing.T) {
	home, bin := writeCodexQueueFixture(t, testQueueUUID)
	runner := &fakeCommandRunner{output: []byte("Usage: --thread --message\n")}
	q := testCodexQueue(home, bin, runner, stubWriterLock{held: false})
	err := q.Inject(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID, "payload")
	if !errors.Is(err, ErrTargetDegraded) {
		t.Fatalf("Inject() error = %v, want ErrTargetDegraded", err)
	}
	if !strings.Contains(err.Error(), "no active writer") {
		t.Fatalf("error = %v, want remedy naming an active writer", err)
	}
	for _, c := range runner.calls {
		if len(c.args) >= 2 && c.args[0] == "queue" && c.args[1] == "--thread" {
			t.Fatalf("idle lock issued a queue inject call: %#v", c)
		}
	}
}

func TestCodexQueueMissingRolloutIsTargetNotFound(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{output: []byte("Usage: --thread --message\n")}
	q := testCodexQueue(home, bin, runner, stubWriterLock{held: true})
	err := q.Probe(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe() error = %v, want ErrTargetNotFound", err)
	}
}

func TestCodexQueueLookPathMissMakesZeroCalls(t *testing.T) {
	home, _ := writeCodexQueueFixture(t, testQueueUUID)
	runner := &fakeCommandRunner{output: []byte("Usage: --thread --message\n")}
	q := CodexQueue{
		Runner:            runner,
		LookPath:          func(string) (string, error) { return "", errors.New("not in PATH") },
		InspectWriterLock: stubWriterLock{held: true},
		Home:              home,
	}
	if err := q.Probe(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID); err == nil {
		t.Fatal("Probe() succeeded with LookPath miss")
	} else if !strings.Contains(err.Error(), "put a codex >= 0.149 first in PATH") {
		t.Fatalf("error = %v, want PATH remedy", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %#v, want zero", runner.calls)
	}
}

func TestCodexQueueHelpWithoutThreadIsRefused(t *testing.T) {
	home, bin := writeCodexQueueFixture(t, testQueueUUID)
	runner := &fakeCommandRunner{output: []byte("Usage: codex exec\n")}
	q := testCodexQueue(home, bin, runner, stubWriterLock{held: true})
	err := q.Probe(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID)
	if err == nil {
		t.Fatal("Probe() succeeded without --thread in help")
	}
	if !strings.Contains(err.Error(), bin) || !strings.Contains(err.Error(), "put a codex >= 0.149 first in PATH") {
		t.Fatalf("error = %v, want resolved path and PATH remedy", err)
	}
	for _, c := range runner.calls {
		if len(c.args) >= 2 && c.args[0] == "queue" && c.args[1] == "--thread" {
			t.Fatalf("contract miss issued a queue inject: %#v", c)
		}
	}
}

func TestCodexQueueNonZeroExitWithoutEnqueueIsReplayable(t *testing.T) {
	home, bin := writeCodexQueueFixture(t, testQueueUUID)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: []byte("Usage: --thread --message\n")},
		{output: []byte("Error: failed to queue session message: thread/queue/add failed: failed to read thread: invalid thread-store request: no rollout found for thread id 00000000-0000-0000-0000-000000000000 (code -32603)\n"), err: errors.New("exit status 1")},
	}}
	q := testCodexQueue(home, bin, runner, stubWriterLock{held: true})
	err := q.Inject(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID, "payload")
	if err == nil {
		t.Fatal("Inject() succeeded on non-zero queue exit")
	}
	if errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() = %v, want replayable error not uncertain", err)
	}
}

func TestCodexQueueNonZeroExitAfterEnqueueIsUncertain(t *testing.T) {
	home, bin := writeCodexQueueFixture(t, testQueueUUID)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: []byte("Usage: --thread --message\n")},
		{output: []byte("Queued message 01a03dc4-f2ae-7fc0-ab04-c507283624b4 for thread " + testQueueUUID + ".\n"), err: errors.New("exit status 1")},
	}}
	q := testCodexQueue(home, bin, runner, stubWriterLock{held: true})
	err := q.Inject(context.Background(), codexQueueTargetThreadPrefix+testQueueUUID, "payload")
	if !errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() error = %v, want ErrInjectUncertain", err)
	}
}
