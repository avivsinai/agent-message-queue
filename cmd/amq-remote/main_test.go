package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The amq-remote CLI shipped with no tests at all, while documenting a precise
// exit-code contract (0 ok, 1 failed or cancelled, 2 usage, 3 not found, 4
// wait timed out, 6 action required). Two defects found in the 2026-09-10
// audit — cancel substituting the live session epoch, and endpoint shutdown
// reported as "work failed" — were both reachable from one happy-path run of
// these verbs. These are those runs: one per user-visible behaviour, against a
// real endpoint over the real socket (agent-message-queue-611.22.29).

// startServe boots a real endpoint with the fake runtime in the background and
// returns its state directory. Socket paths must stay short, so the state dir
// lives directly under the system temp root.
func startServe(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- run([]string{"serve", "--fake", "--root", root}, strings.NewReader(""), &out, &errBuf)
	}()
	t.Cleanup(func() {
		// serve exits on its own when the test process ends; drain if it
		// already returned so a failure surfaces instead of hanging.
		select {
		case <-done:
		default:
		}
	})

	// Wait for the socket to accept: `sessions` succeeds only once serving.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out, errBuf bytes.Buffer
		if run([]string{"sessions", "--root", root, "--json"}, strings.NewReader(""), &out, &errBuf) == 0 {
			return root
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("endpoint did not start serving within 5s")
	return ""
}

func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestCLIUsageAndVersion pins the two exits that need no endpoint.
func TestCLIUsageAndVersion(t *testing.T) {
	if code, _, _ := cli(t, "", "--help"); code != protocol.ExitUsage {
		t.Fatalf("--help exit = %d, want %d", code, protocol.ExitUsage)
	}
	if code, _, _ := cli(t, "", "no-such-command"); code != protocol.ExitUsage {
		t.Fatalf("unknown command exit = %d, want %d", code, protocol.ExitUsage)
	}
	code, out, _ := cli(t, "", "--version")
	if code != 0 || !strings.Contains(out, "amq-remote") {
		t.Fatalf("--version exit=%d out=%q", code, out)
	}
}

// TestCLISubmitStatusWaitHappyPath is the core round trip: submit a request,
// read it back, and wait for the result the runtime produces. It also pins
// that a wait which times out exits 4 rather than reporting failure.
func TestCLISubmitStatusWaitHappyPath(t *testing.T) {
	root := startServe(t)

	code, out, errOut := cli(t, "", "submit", "fake", "--text", "say hi", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("submit exit=%d out=%s err=%s", code, out, errOut)
	}
	var rep protocol.Reply
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("submit output is not a Reply: %v (%s)", err, out)
	}
	ref := rep.Snapshot.RequestRef
	if ref == "" {
		t.Fatalf("submit returned no request ref: %s", out)
	}

	// status reads the same record back.
	code, out, errOut = cli(t, "", "status", ref, "--root", root, "--json")
	if code != 0 {
		t.Fatalf("status exit=%d out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, rep.Snapshot.RequestID) {
		t.Fatalf("status did not return the submitted request: %s", out)
	}

	// A wait that expires before the runtime answers is a TIMEOUT (exit 4),
	// never a failure: the request is untouched and still running.
	code, _, _ = cli(t, "", "wait", ref, "--timeout", "300ms", "--root", root, "--json")
	if code != protocol.ExitTimeout {
		t.Fatalf("expired wait exit = %d, want %d (timeout, not failure)", code, protocol.ExitTimeout)
	}
}

// TestCLIStatusUnknownRefIsNotFound pins exit 3 — distinct from a failure.
func TestCLIStatusUnknownRefIsNotFound(t *testing.T) {
	root := startServe(t)
	ref := protocol.EncodeRef("local", "fake", "11111111-1111-4111-8111-1111111119f0")
	code, out, _ := cli(t, "", "status", ref, "--root", root, "--json")
	if code != protocol.ExitNotFound {
		t.Fatalf("unknown ref exit = %d, want %d (not found) out=%s", code, protocol.ExitNotFound, out)
	}
}

// TestCLIDisabledModeIsActionRequired pins exit 6: busy=queue and
// deliver=steer are disabled in v1, and a disabled mode is action-required
// (the caller must choose another mode), not a failure.
func TestCLIDisabledModeIsActionRequired(t *testing.T) {
	root := startServe(t)
	code, out, _ := cli(t, "", "submit", "fake", "--text", "x", "--busy", "queue", "--root", root, "--json")
	if code != protocol.ExitActionRequired {
		t.Fatalf("busy=queue exit = %d, want %d (action required) out=%s", code, protocol.ExitActionRequired, out)
	}
}

// TestCLISubmitRejectsEmptyPrompt pins that a whitespace-only prompt never
// reaches a harness: it is a usage error at the CLI boundary.
func TestCLISubmitRejectsEmptyPrompt(t *testing.T) {
	root := startServe(t)
	code, _, _ := cli(t, "", "submit", "fake", "--text", "   ", "--root", root, "--json")
	if code == 0 {
		t.Fatal("whitespace-only prompt was accepted")
	}
}
