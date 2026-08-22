package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testMe = "cursor"
	testTo = "codex"
)

// builtBinaries builds amq and amq-acp once per test binary run so the
// end-to-end cases exercise the real commands rather than in-process shortcuts.
var builtBinaries struct {
	once sync.Once
	dir  string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBinaries.dir != "" {
		_ = os.RemoveAll(builtBinaries.dir)
	}
	os.Exit(code)
}

func binaries(t *testing.T) (amq string, amqACP string) {
	t.Helper()
	builtBinaries.once.Do(func() {
		dir, err := os.MkdirTemp("", "amq-acp-binaries-")
		if err != nil {
			builtBinaries.err = err
			return
		}
		builtBinaries.dir = dir
		for _, target := range []string{"./cmd/amq", "./cmd/amq-acp"} {
			if err := buildCommand(dir, target); err != nil {
				builtBinaries.err = err
				return
			}
		}
	})
	if builtBinaries.err != nil {
		t.Fatalf("build test binaries: %v", builtBinaries.err)
	}
	return filepath.Join(builtBinaries.dir, "amq"), filepath.Join(builtBinaries.dir, "amq-acp")
}

func buildCommand(dir, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := filepath.Base(target)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, name), target)
	cmd.Dir = repoRoot()
	cmd.Env = cleanEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s: %w\n%s", target, err, output)
	}
	return nil
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// cleanEnv strips inherited AMQ context so a developer's own pinned shell cannot
// influence a test.
func cleanEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			name = entry[:i]
		}
		switch name {
		case "AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION", "AM_ME",
			"AMQ_ACP_TO", "AMQ_GLOBAL_ROOT", "AMQ_WAKE_OWNER":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "AMQ_NO_UPDATE_CHECK=1")
}

// initQueue creates a real queue root with both handles registered.
func initQueue(t *testing.T, amq string) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command(amq, "init", "--root", root, "--agents", testMe+","+testTo)
	cmd.Env = cleanEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("amq init: %v\n%s", err, output)
	}
	return root
}

// acpSession drives a live amq-acp process over its stdio pipes.
type acpSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *strings.Builder
}

func startACP(t *testing.T, binary string, env ...string) *acpSession {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = append(cleanEnv(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start amq-acp: %v", err)
	}
	session := &acpSession{t: t, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return session
}

// call sends one request and returns the decoded response result.
func (s *acpSession) call(request string) map[string]json.RawMessage {
	s.t.Helper()
	if _, err := fmt.Fprintln(s.stdin, request); err != nil {
		s.t.Fatalf("write request: %v (stderr: %s)", err, s.stderr)
	}
	line, err := s.stdout.ReadString('\n')
	if err != nil {
		s.t.Fatalf("read response: %v (stderr: %s)", err, s.stderr)
	}
	var envelope struct {
		Result map[string]json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		s.t.Fatalf("decode response %q: %v", line, err)
	}
	if envelope.Error != nil {
		s.t.Fatalf("request %s failed: %d %s", request, envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Result
}

func (s *acpSession) closeAndWait() {
	s.t.Helper()
	if err := s.stdin.Close(); err != nil {
		s.t.Fatalf("close stdin: %v", err)
	}
	if err := s.cmd.Wait(); err != nil {
		s.t.Fatalf("amq-acp exited with error: %v (stderr: %s)", err, s.stderr)
	}
}

// TestACPPromptIsDrainableByRealAMQ is the end-to-end proof: a prompt sent
// through the real amq-acp binary becomes a message the real amq drain command
// consumes, body included.
func TestACPPromptIsDrainableByRealAMQ(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary end-to-end")
	}
	amq, amqACP := binaries(t)
	root := initQueue(t, amq)

	session := startACP(t, amqACP, "AM_ROOT="+root, "AM_ME="+testMe, "AMQ_ACP_TO="+testTo)

	initialized := session.call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true}}}`)
	if version := string(initialized["protocolVersion"]); version != "1" {
		t.Fatalf("protocolVersion = %s over real stdio, want 1", version)
	}

	created := session.call(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"` + root + `","mcpServers":[]}}`)
	sessionID := unquote(t, created["sessionId"])
	if sessionID == "" {
		t.Fatal("session/new returned no sessionId")
	}

	const body = "acp preview handoff: review internal/acp"
	prompted := session.call(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + sessionID + `","prompt":[{"type":"text","text":"` + body + `"}]}}`)
	if reason := unquote(t, prompted["stopReason"]); reason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", reason)
	}
	session.closeAndWait()

	drained := drainJSON(t, amq, root, testTo)
	if drained.Count != 1 {
		t.Fatalf("amq drain returned count %d, want 1", drained.Count)
	}
	item := drained.Drained[0]
	if strings.TrimSpace(item.Body) != body {
		t.Errorf("drained body = %q, want %q", item.Body, body)
	}
	if item.From != testMe {
		t.Errorf("drained from = %q, want %q", item.From, testMe)
	}
	if item.Thread != "p2p/codex__cursor" {
		t.Errorf("drained thread = %q, want p2p/codex__cursor", item.Thread)
	}
	if !item.MovedToCur {
		t.Error("drained message was not moved to cur")
	}

	// A second drain must find nothing: the first one really consumed it.
	if again := drainJSON(t, amq, root, testTo); again.Count != 0 {
		t.Errorf("second drain returned count %d, want 0", again.Count)
	}
}

type drainOutput struct {
	Count   int `json:"count"`
	Drained []struct {
		ID         string `json:"id"`
		From       string `json:"from"`
		Thread     string `json:"thread"`
		Subject    string `json:"subject"`
		Body       string `json:"body"`
		MovedToCur bool   `json:"moved_to_cur"`
	} `json:"drained"`
}

func drainJSON(t *testing.T, amq, root, me string) drainOutput {
	t.Helper()
	cmd := exec.Command(amq, "drain", "--root", root, "--me", me, "--include-body", "--json")
	cmd.Env = cleanEnv()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("amq drain: %v\n%s", err, output)
	}
	var result drainOutput
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode drain output %q: %v", output, err)
	}
	return result
}

// TestBinaryRefusesMissingRoot proves the companion fails closed instead of
// inventing a queue root.
func TestBinaryRefusesMissingRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary end-to-end")
	}
	_, amqACP := binaries(t)

	code, stderr := runACPExpectingFailure(t, amqACP, nil, "AM_ME="+testMe, "AMQ_ACP_TO="+testTo)
	if code != exitContextMismatch {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitContextMismatch, stderr)
	}
	if !strings.Contains(stderr, "AM_ROOT") {
		t.Errorf("stderr %q does not name the missing variable", stderr)
	}
}

// TestBinaryRefusesContradictorySessionPin proves an inherited pin is honored:
// a root outside the pinned session is refused before any delivery.
func TestBinaryRefusesContradictorySessionPin(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary end-to-end")
	}
	amq, amqACP := binaries(t)
	root := initQueue(t, amq)
	base := t.TempDir()

	code, stderr := runACPExpectingFailure(t, amqACP, nil,
		"AM_ROOT="+root,
		"AM_BASE_ROOT="+base,
		"AM_SESSION=session1",
		"AM_ME="+testMe,
		"AMQ_ACP_TO="+testTo,
	)
	if code != exitContextMismatch {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitContextMismatch, stderr)
	}

	inboxNew := filepath.Join(root, "agents", testTo, "inbox", "new")
	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		t.Fatalf("read %s: %v", inboxNew, err)
	}
	if len(entries) != 0 {
		t.Errorf("inbox holds %d messages after a refused pin, want 0", len(entries))
	}
}

func TestBinaryRejectsUnknownFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary end-to-end")
	}
	_, amqACP := binaries(t)

	code, _ := runACPExpectingFailure(t, amqACP, []string{"--listen"}, "AM_ROOT=/tmp", "AM_ME="+testMe, "AMQ_ACP_TO="+testTo)
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

func TestBinaryRejectsBuzzDefaultACPArg(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary end-to-end")
	}
	_, amqACP := binaries(t)

	code, stderr := runACPExpectingFailure(t, amqACP, []string{"acp"}, "AM_ROOT=/tmp", "AM_ME="+testMe, "AMQ_ACP_TO="+testTo)
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "unexpected arguments") {
		t.Fatalf("stderr = %q, want unexpected arguments", stderr)
	}
}

func TestBinaryPrintsVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary end-to-end")
	}
	_, amqACP := binaries(t)

	cmd := exec.Command(amqACP, "--version")
	cmd.Env = cleanEnv()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("amq-acp --version: %v", err)
	}
	if strings.TrimSpace(string(output)) == "" {
		t.Error("amq-acp --version printed nothing")
	}
}

// runACPExpectingFailure runs the companion with an empty stdin and returns its
// exit code and stderr.
func runACPExpectingFailure(t *testing.T, binary string, args []string, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(cleanEnv(), env...)
	cmd.Stdin = strings.NewReader("")
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("amq-acp succeeded, want a refusal (stderr: %s)", stderr)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("amq-acp failed without an exit status: %v", err)
	}
	return exitErr.ExitCode(), stderr.String()
}

func unquote(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode string %q: %v", raw, err)
	}
	return value
}
