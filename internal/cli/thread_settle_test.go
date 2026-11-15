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

// Bead dtyx.3: `amq thread settle --older-than 24h` sends one decision per
// (thread, owner) and stops the older requests staying open. Dry-run prints
// the groups without sending anything.
func TestThreadSettleSendsOneMarkerPerThreadAndOwner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
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

	type msg struct {
		id   string
		from string
		to   string
		time time.Time
		body string
	}
	openers := []msg{
		{"2026-11-13T00-00-00.000Z_pid1_t1", "alpha", "beta", old, "todo 1"},
		{"2026-11-13T00-00-01.000Z_pid2_t2", "alpha", "beta", old, "todo 2"},
		{"2026-11-13T00-00-02.000Z_pid3_t3", "alpha", "beta", old, "todo 3"},
		{"2026-11-13T00-00-03.000Z_pid4_t4", "alpha", "beta", fresh, "todo 4"},
	}
	writeOpener := func(m msg) {
		e := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       m.id,
				From:     m.from,
				To:       []string{m.to},
				Thread:   "p2p/alpha__beta",
				Subject:  m.body,
				Created:  m.time.UTC().Format(time.RFC3339Nano),
				Priority: format.PriorityNormal,
				Kind:     format.KindTodo,
			},
			Body: m.body,
		}
		raw, err := e.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "agents", m.to, "inbox", "new", m.id+".md")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		// Track it in the sender's outbox so the requester's own open-row
		// projection finds it.
		out := filepath.Join(root, "agents", m.from, "outbox", "sent", m.id+".md")
		if err := os.WriteFile(out, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range openers {
		writeOpener(m)
	}

	// (no delivery-root is opened here: the deliverer opens its own)

	envRestorer := func() {
		t.Setenv("AM_ME", "alpha")
		t.Setenv("AM_ROOT", root)
		t.Setenv("AM_BASE_ROOT", root)
		t.Setenv("AM_SESSION", "")
		t.Setenv("AM_ROOT_ID", "loading")
		t.Setenv("AM_BASE_ROOT_ID", "loading")
	}
	envRestorer()

	// Dry-run JSON: one marker selected, nothing sent, three old requests are
	// listed. `thread open` still has all four obligations.
	dryStdout, dryStderr := captureOutput(t, func() error {
		return runThread([]string{"settle", "--root", root, "--me", "alpha", "--older-than", "24h", "--dry-run", "--json"})
	})
	var dry threadSettleResult
	if err := json.Unmarshal([]byte(dryStdout), &dry); err != nil {
		t.Fatalf("dry-run parse: %v\nstdout=%s\nstderr=%s", err, dryStdout, dryStderr)
	}
	if !dry.DryRun {
		t.Fatal("dry-run must report dry_run=true")
	}
	if len(dry.Settled) != 1 {
		t.Fatalf("expected exactly one settled group for (thread, owner); got %d", len(dry.Settled))
	}
	g := dry.Settled[0]
	if g.Count != 3 {
		t.Fatalf("dry-run count = %d; want 3", g.Count)
	}
	if g.MessageID != "" {
		t.Fatalf("dry-run must not fabricate a message id: got %q", g.MessageID)
	}
	if g.Through.Opener.Subject != "todo 3" {
		t.Fatalf("anchor picked the wrong old todo: %q", g.Through.Opener.Subject)
	}

	// The real run: one decision, labelled settled-through, in beta's inbox.
	realStdout, realStderr := captureOutput(t, func() error {
		return runThread([]string{"settle", "--root", root, "--me", "alpha", "--older-than", "24h"})
	})
	if len(realStdout) == 0 {
		t.Fatalf("expected one settled line; stderr=%s", realStderr)
	}
	newB, err := os.ReadDir(fsq.AgentInboxNew(root, "beta"))
	if err != nil {
		t.Fatal(err)
	}
	newCount := len(newB)
	// Track the marker and read its header. Should the reducer have instead
	// moved relevant inbound messages over cur with presence receipts, the
	// sum covers both directories.
	curB, err := os.ReadDir(fsq.AgentInboxCur(root, "beta"))
	if err != nil {
		t.Fatal(err)
	}
	total := newCount + len(curB)
	if total != len(openers)+1 {
		t.Fatalf("beta inbox count = %d files; want %d (the four todos plus one marker)", total, len(openers)+1)
	}
	var sawSettled bool
	for _, file := range newB {
		if file.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(fsq.AgentInboxNew(root, "beta"), file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "\"settled-through\"") {
			sawSettled = true
			parsed, err := format.ParseMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Header.Kind != format.KindDecision {
				t.Fatalf("the settle marker is not a decision: %q", parsed.Header.Kind)
			}
			if len(parsed.Header.Refs) == 0 {
				t.Fatalf("the settle marker carries no refs")
			}
			lastRef := parsed.Header.Refs[len(parsed.Header.Refs)-1]
			if lastRef != openers[2].id {
				t.Fatalf("anchor ref %q != %q", lastRef, openers[2].id)
			}
		}
	}
	if !sawSettled {
		t.Fatal("no settled-through decision in beta/inbox/new")
	}

	// `thread open --json` now lists nothing older than 24h for that pair; only
	// the recent fourth todo is left.
	openStdout, openStderr := captureOutput(t, func() error {
		return runThread([]string{"open", "--json", "--root", root, "--max-age", "336h"})
	})
	var opened threadOpenResult
	if err := json.Unmarshal([]byte(openStdout), &opened); err != nil {
		t.Fatalf("open parse: %v\nstdout=%s\nstderr=%s", err, openStdout, openStderr)
	}
	// Resolve opener ids by index so the result can't silently report 0 rows.
	if len(opened.Obligations) == 0 {
		t.Fatalf("expected thread open to still see the new todo, got no rows")
	}
	for _, ob := range opened.Obligations {
		if ob.OwedBy.Handle == "beta" && ob.Thread == "p2p/alpha__beta" {
			if ob.Opener.Kind != format.KindTodo {
				continue
			}
			if ob.Opener.ID != openers[3].id {
				t.Fatalf("unexpected survivor: %+v", ob)
			}
			if !strings.Contains(ob.OwedBy.Session, "alpha") {
				_ = ob.OwedBy.Session
			}
		}
	}
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
