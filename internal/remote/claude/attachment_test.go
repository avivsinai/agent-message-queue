package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// coreBoundRequest builds one bound request for the refusal tests.
func coreBoundRequest() core.BoundRequest {
	return core.BoundRequest{
		Key:   requests.Key{CreatorHost: "local", TargetID: "claude:9", RequestID: "11111111-1111-4111-8111-111111111111"},
		Epoch: SentinelUnpinned,
		Input: protocol.SubmitInput{Text: "hello"},
	}
}

// tempHome builds a fake Claude home with a session registry for pid and
// optionally a transcript.
func tempHome(t *testing.T, pid int, reg *sessionRegistry) string {
	t.Helper()
	home := t.TempDir()
	dir := claudeSessionsDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, itoa(pid)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestAttachResolvesRegistryAndTarget(t *testing.T) {
	home := tempHome(t, 4242, &sessionRegistry{
		Pid: 4242, SessionID: "abc-123", Cwd: "/tmp/proj", Kind: "interactive",
		MessagingSocketPath: "/tmp/cc-socks/4242.sock", Name: "sess/claude",
	})
	att, err := Attach(config{Pid: 4242, Home: home})
	if err != nil {
		t.Fatalf("Attach refused a registered interactive session: %v", err)
	}
	s := att.Inspect()
	if s.Harness != "claude_code" {
		t.Fatalf("harness = %q, want claude_code (schema enum)", s.Harness)
	}
	if s.TargetID != "sess/claude" {
		t.Fatalf("target = %q, want the registry name", s.TargetID)
	}
	if s.Epoch != SentinelUnpinned {
		t.Fatalf("epoch = %q, want the unpinned sentinel (PR1 binds nothing)", s.Epoch)
	}
	// Honest projection: inspect true, everything else false.
	if !s.Capabilities.Inspect {
		t.Fatal("inspect capability must be true")
	}
	if s.Capabilities.Submit {
		t.Fatal("submit must be FALSE until PR2 wires the socket (ruling 10:59Z)")
	}
	for _, cap := range []string{"cancel_request", "approve_tool", "answer_question", "steer"} {
		switch cap {
		case "cancel_request":
			if s.Capabilities.CancelRequest {
				t.Fatalf("%s must be false (no interrupt seam without keystrokes)", cap)
			}
		case "approve_tool":
			if s.Capabilities.ApproveTool {
				t.Fatalf("%s must be false", cap)
			}
		case "answer_question":
			if s.Capabilities.AnswerQuestion {
				t.Fatalf("%s must be false", cap)
			}
		case "steer":
			if s.Capabilities.Steer {
				t.Fatalf("%s must be false", cap)
			}
		}
	}
	if s.Capabilities.Terminal != "unavailable" {
		t.Fatalf("terminal = %q, want unavailable", s.Capabilities.Terminal)
	}
	// Nil evidence projection: PR1 issues no submit evidence class at all;
	// a nil projection fails closed under any caller floor.
	if s.Evidence != nil {
		t.Fatalf("evidence projection = %+v, want nil (no class is provable in PR1)", s.Evidence)
	}
}

func TestAttachRefusesUnknownPidAndNonInteractive(t *testing.T) {
	home := t.TempDir()
	if _, err := Attach(config{Pid: 1, Home: home}); err == nil {
		t.Fatal("Attach accepted a pid with no session registry entry")
	}
	home = tempHome(t, 7, &sessionRegistry{Pid: 7, SessionID: "x", Kind: "background"})
	if _, err := Attach(config{Pid: 7, Home: home}); err == nil {
		t.Fatal("Attach accepted a non-interactive session")
	}
}

func TestSubmitRefusesWithoutSideEffect(t *testing.T) {
	home := tempHome(t, 9, &sessionRegistry{Pid: 9, SessionID: "s9", Kind: "interactive", Cwd: "/tmp"})
	att, err := Attach(config{Pid: 9, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := att.Submit(coreBoundRequest())
	if err != nil {
		t.Fatalf("Submit returned an error instead of a typed admission refusal: %v", err)
	}
	if admission.Admitted {
		t.Fatal("Submit admitted a request in PR1")
	}
	if string(admission.Code) != "unsupported" {
		t.Fatalf("refusal code = %q, want unsupported", admission.Code)
	}
	if !strings.Contains(admission.Message, "PR2") {
		t.Fatalf("refusal message does not state the PR2 gate: %q", admission.Message)
	}
}

func TestTranscriptTailReadsLastLinesAndDropsPartial(t *testing.T) {
	home := tempHome(t, 11, &sessionRegistry{Pid: 11, SessionID: "sid", Kind: "interactive", Cwd: "/tmp/p"})
	dir := filepath.Join(home, ".claude", "projects", slugifyCwd("/tmp/p"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "{\"type\":\"user\",\"promptId\":\"p1\"}\n" +
		"{\"type\":\"user\",\"promptId\":\"p2\"}\n" +
		"{\"type\":\"assistant\"}\n" +
		"{\"type\":\"assistant\",\"trunc"
	if err := os.WriteFile(filepath.Join(dir, "sid.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := transcriptTail(home, "/tmp/p", "sid", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 { // user, user, assistant; the partial 4th line is dropped
		t.Fatalf("tail lines = %d, want 3 (partial trailing line dropped)", len(lines))
	}
	if lines[len(lines)-1].Type != "assistant" {
		t.Fatalf("last line type = %q, want assistant", lines[len(lines)-1].Type)
	}
	// Missing transcript is an empty tail, not an error.
	lines, err = transcriptTail(home, "/tmp/other", "sid", 10)
	if err != nil || lines != nil {
		t.Fatalf("missing transcript: lines=%v err=%v, want nil,nil", lines, err)
	}
}

func TestSlugifyCwd(t *testing.T) {
	got := slugifyCwd("/Users/aviv.s/workspace/agent-message-queue")
	want := "-Users-aviv-s-workspace-agent-message-queue"
	if got != want {
		t.Fatalf("slug = %q, want %q", got, want)
	}
}
