package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestAttachResolvesRegistryAndTarget(t *testing.T) {
	home := tempHome(t, 4242, &sessionRegistry{
		Pid: 4242, SessionID: "abc-123", Cwd: "/tmp/proj", Kind: "interactive",
		MessagingSocketPath: "/tmp/cc-socks/4242.sock", Name: "sess/claude",
	})
	// cfg.Target wins (P0-3): the manifest target is the identity the
	// endpoint addresses the adapter under; the registry name crosses a
	// trust boundary (local-process-writable file) and is only a fallback.
	att, err := Attach(config{Pid: 4242, Home: home, Target: "cc-1"})
	if err != nil {
		t.Fatalf("Attach refused a registered interactive session: %v", err)
	}
	s := att.Inspect()
	if s.Harness != "claude_code" {
		t.Fatalf("harness = %q, want claude_code (schema enum)", s.Harness)
	}
	if s.TargetID != "cc-1" {
		t.Fatalf("target = %q, want the manifest target cc-1 (P0-3)", s.TargetID)
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

func TestDeriveTargetValidatesAndFallsBack(t *testing.T) {
	// A protocol-invalid registry name falls back to the pid form (P0-3:
	// the published id must always be addressable — ValidTargetID).
	got := deriveTarget(4242, &sessionRegistry{Name: "sess/claude"})
	if got != "claude:4242" {
		t.Fatalf("deriveTarget with invalid name = %q, want claude:4242", got)
	}
	if got := deriveTarget(4242, &sessionRegistry{Name: "cc-1"}); got != "cc-1" {
		t.Fatalf("deriveTarget with valid name = %q, want cc-1", got)
	}
	// Attach refuses an unvalidatable derived target rather than publishing it.
	home := tempHome(t, 5, &sessionRegistry{Pid: 5, SessionID: "s5", Kind: "interactive"})
	// "claude:5" is protocol-valid (letters, digits, ':' allowed), so a
	// plain pid fallback must attach and publish the pid form.
	att, err := Attach(config{Pid: 5, Home: home})
	if err != nil {
		t.Fatalf("Attach refused the pid-form fallback: %v", err)
	}
	if s := att.Inspect(); s.TargetID != "claude:5" {
		t.Fatalf("target = %q, want claude:5", s.TargetID)
	}
}

func TestNormalizeStatusProjectionVocabulary(t *testing.T) {
	// P2-1: the registry's foreign vocabulary is never published raw.
	for _, tc := range []struct{ in, want string }{
		{"idle", "idle"}, {"busy", "busy"}, {"shell", "busy"}, {"tool", "busy"},
		{"waitingForUserInput", "unknown"}, {"", "unknown"}, {"hostile:value", "unknown"},
	} {
		if got := normalizeStatus(tc.in); got != tc.want {
			t.Fatalf("normalizeStatus(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRegistryLeafMustBeRegularFile pins r2 P1-1: the session-registry path
// is local-process-writable; a non-regular file there (FIFO, directory) is
// refused by lstat BEFORE any read — os.ReadFile on a FIFO blocks
// unbounded, and this read runs under the endpoint's mutex.
func TestRegistryLeafMustBeRegularFile(t *testing.T) {
	home := tempHome(t, 77, &sessionRegistry{Pid: 77, SessionID: "s77", Kind: "interactive"})
	regPath := filepath.Join(claudeSessionsDir(home), "77.json")
	// Directory at the leaf: refused, never read.
	if err := os.Remove(regPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(regPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionRegistry(home, 77); err == nil {
		t.Fatal("accepted a directory at the session-registry path")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("refusal does not name the file-type rule: %v", err)
	}
	testRegistryFIFO(t, home, regPath)
}

// TestRegistryOversizedRefusedBeforeRead pins verifier r3 P1: a registry
// file over the 64 KiB bound is refused on fi.Size() BEFORE any read —
// no os.ReadFile of a multi-GB file under the endpoint mutex — and the
// LimitReader caps the lstat-to-read growth window (r3 P2-2).
func TestRegistryOversizedRefusedBeforeRead(t *testing.T) {
	home := tempHome(t, 42, &sessionRegistry{Pid: 42, SessionID: "s42", Kind: "interactive"})
	regPath := filepath.Join(claudeSessionsDir(home), "42.json")
	if err := os.Truncate(regPath, maxRegistryBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionRegistry(home, 42); err == nil {
		t.Fatal("accepted an oversized session registry (r3 P1)")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("refusal does not state the size bound: %v", err)
	}
	// Just under the bound parses fine.
	if err := os.Truncate(regPath, maxRegistryBytes-1); err != nil {
		t.Fatal(err)
	}
	// Truncated to a non-JSON blob is a parse error, NOT a size refusal —
	// proving the read path runs for in-bound files.
	if _, err := readSessionRegistry(home, 42); err == nil {
		t.Fatal("expected a JSON parse error for the truncated blob")
	} else if strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("in-bound file was size-refused: %v", err)
	}
}
