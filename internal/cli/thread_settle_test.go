package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type testOpener struct{ id, from, to, kind, subject, created string }

// Bead dtyx.3: `amq thread settle` runs the same fixture in two real
// modes, back to back, before `--dry-run`. First invocation is REAL --json,
// second is text; this is how we pin the stream shape the live run broke.
func TestThreadSettleSendsOneMarkerPerThreadAndOwner(t *testing.T) {
	for _, jsonMode := range []bool{true, false} {
		root, openers := buildSettleFixture(t)
		if jsonMode {
			stdout, _ := captureOutput(t, func() error {
				return runThread([]string{"settle", "--root", root, "--me", "alpha", "--older-than", "24h", "--json"})
			})
			var result threadSettleResult
			if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result); err != nil {
				t.Fatalf("real --json emitted text or noise alongside the object: %v\nstdout first 15 lines:\n%s", err, firstNLines(stdout, 15))
			}
			if len(result.Settled) != 1 {
				t.Fatalf("real --json must group exactly one sender/owner pair: got %d", len(result.Settled))
			}
			entry := result.Settled[0]
			if entry.Count != 3 || entry.MessageID == "" {
				t.Fatalf("real --json does not name the same three old requests or carry a real id: %+v", entry)
			}
			assertSettleDeliveryProves(t, root, openers, entry.MessageID)
			// The new obligations are still owed.
			expectOnlyNewObligation(t, root, openers[3].id)
		} else {
			stdout, _ := captureOutput(t, func() error {
				return runThread([]string{"settle", "--root", root, "--me", "alpha", "--older-than", "24h"})
			})
			if strings.Contains(stdout, "{") {
				t.Fatalf("text output includes JSON prefix:\n%s", stdout)
			}
			if !strings.Contains(stdout, "settled 3 requests to beta in p2p/alpha__beta through 2026-11-13T00-00-02.000Z_pid3_t3 (\"review 1\") as ") {
				t.Fatalf("text shape wrong:\n%s", stdout)
			}
			if !strings.Contains(stdout, "kept: 1 request newer than 24h") {
				t.Fatalf("singular kept form expected: %s", stdout)
			}
			assertSettleDeliveryProves(t, root, openers, "")
			expectOnlyNewObligation(t, root, openers[3].id)
		}
	}
}

func firstNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// assertSettleDeliveryProves checks the marker is physically written in the
// two documented places, receipt status doesn't matter on an explicit route.
func assertSettleDeliveryProves(t *testing.T, root string, openers []testOpener, wantID string) {
	t.Helper()
	ownerFiles, err := filepath.Glob(filepath.Join(fsq.AgentInboxNew(root, "beta"), "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantLen := len(openers) + 1
	if len(ownerFiles) != wantLen {
		t.Fatalf("owner inbox count = %d; want %d (the four todos + the marker)", len(ownerFiles), wantLen)
	}
	for _, path := range ownerFiles {
		if wantID != "" && filepath.Base(path) != wantID+".md" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var header format.Message
		parsed, err := format.ParseMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		header = parsed
		if header.Header.Kind == format.KindDecision && strings.Contains(string(raw), "settled-through") {
			return
		}
		_ = header
	}
	t.Fatal("no settled-through decision in beta/inbox/new")
}

func expectOnlyNewObligation(t *testing.T, root, id string) {
	t.Helper()
	stdout, errout := captureOutput(t, func() error {
		return runThread([]string{"open", "--json", "--root", root, "--max-age", "336h"})
	})
	var opened threadOpenResult
	if err := json.Unmarshal([]byte(stdout), &opened); err != nil {
		t.Fatalf("thread open output: %v\nstdout=%s\nstderr=%s", err, stdout, errout)
	}
	for _, ob := range opened.Obligations {
		if ob.OwedBy.Handle == "beta" && ob.Thread == "p2p/alpha__beta" {
			if ob.Opener.ID != id && ob.State != "closed" {
				t.Fatalf("unexpected survivor: %+v", ob)
			}
		}
	}
}

func buildSettleFixture(t *testing.T) (root string, openers []testOpener) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "queue")
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"alpha", "beta"} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	fresh := time.Now().UTC().Add(-time.Second)
	type in struct {
		id, kind, subject string
		createdAt         time.Time
	}
	openers = []testOpener{}
	wants := []in{
		{"2026-11-13T00-00-00.000Z_pid1_t1", format.KindTodo, "todo 1", old},
		{"2026-11-13T00-00-01.000Z_pid2_t2", format.KindTodo, "todo 2", old},
		{"2026-11-13T00-00-02.000Z_pid3_t3", format.KindReviewRequest, "review 1", old},
		{"2026-11-13T00-00-03.000Z_pid4_t4", format.KindTodo, "todo 4", fresh},
	}
	for _, in := range wants {
		e := format.Message{Header: format.Header{
			Schema: format.CurrentSchema,
			ID:     in.id, From: "alpha", To: []string{"beta"}, Thread: "p2p/alpha__beta",
			Subject: in.subject, Created: in.createdAt.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal, Kind: in.kind}}
		raw, err := e.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "agents", "beta", "inbox", "new", in.id+".md"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "agents", "alpha", "outbox", "sent", in.id+".md"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		openers = append(openers, testOpener{id: in.id, from: "alpha", to: "beta", kind: in.kind, subject: in.subject, created: in.createdAt.UTC().Format(time.RFC3339Nano)})
	}
	return root, openers
}

// Bead dtyx.3 lead ruling 1: the plaintext thread-open view summarizes
// ambiguous replies once per sender with a count.
func TestThreadOpenPlaintextAmbiguousBySenderOneLine(t *testing.T) {
	root := filepath.Join(t.TempDir(), defaultCoopRoot, "s1")
	messages := []threadOpenMessage{
		{name: "p", from: "beta@s1", to: "alpha@s1", thread: "x", kind: format.KindTodo, refs: []string{"l"}, at: 20 * time.Second},
		{name: "s1", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"p"}, thread: "x", at: 21 * time.Second},
		{name: "s2", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"p"}, thread: "x", at: 22 * time.Second},
		{name: "s3", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"p"}, thread: "x", at: 23 * time.Second},
		{name: "l", from: "beta@s1", to: "alpha@s1", thread: "x", kind: format.KindTodo, at: 40 * time.Second},
	}
	writeThreadOpenMessages(t, filepath.Dir(root), messages, func(i int, m threadOpenMessage) time.Time {
		return time.Now().UTC().Add(m.at)
	})
	sout, _ := captureOutput(t, func() error {
		return runThread([]string{"open", "--root", root})
	})
	got := 0
	for _, line := range strings.Split(sout, "\n") {
		if strings.HasPrefix(line, "ambiguous:") {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("ambiguous summary lines = %d; want 2 (one for each sender)\n%s", got, sout)
	}
	if !strings.Contains(sout, "3 messages from alpha") || !strings.Contains(sout, "1 messages from beta") {
		t.Fatalf("summary lines must carry the sender and its count:\n%s", sout)
	}
	if !strings.Contains(sout, "see --json") {
		t.Fatalf("summary line must point at the JSON output:\n%s", sout)
	}
}

// A request from another project's agent that has my handle is not mine to
// settle. Seen in the live run of 0.146.0 on 2026-11-15: the command
// selected by handle alone and offered to settle requests that an agent
// named like the caller, in two other projects, had asked.
func TestThreadSettleLeavesAnotherProjectsRequestsWithMyHandle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"alpha", "beta"} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	id := "2026-11-12T00-00-00.000Z_pid1_other"
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "alpha", To: []string{"beta"},
		ReplyProject: "other-project", Thread: "p2p/other-project:alpha__beta",
		Subject:  "asked by the other project's alpha",
		Created:  time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano),
		Priority: format.PriorityNormal, Kind: format.KindTodo,
	}}
	raw, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	inbox := fsq.AgentInboxNew(root, "beta")
	if err := os.WriteFile(filepath.Join(inbox, id+".md"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The request is open, and its requester is the other project's alpha.
	openOut, _ := captureOutput(t, func() error { return runThread([]string{"open", "--json", "--root", root}) })
	var open threadOpenResult
	if err := json.Unmarshal([]byte(openOut), &open); err != nil {
		t.Fatalf("thread open: %v\n%s", err, openOut)
	}
	if len(open.Obligations) != 1 || open.Obligations[0].OwedTo.Project != "other-project" {
		t.Fatalf("fixture: want one open request owed to other-project's alpha, got %s", openOut)
	}

	stdout, _ := captureOutput(t, func() error {
		return runThread([]string{"settle", "--root", root, "--me", "alpha", "--older-than", "0"})
	})
	if stdout != "Nothing to settle.\n" {
		t.Fatalf("settle stdout = %q, want %q", stdout, "Nothing to settle.\n")
	}
	if sent, _ := filepath.Glob(filepath.Join(fsq.AgentOutboxSent(root, "alpha"), "*.md")); len(sent) != 0 {
		t.Fatalf("alpha sent %d messages, want none", len(sent))
	}
	if got, _ := filepath.Glob(filepath.Join(inbox, "*.md")); len(got) != 1 {
		t.Fatalf("beta's inbox holds %d messages, want only the request", len(got))
	}
}
