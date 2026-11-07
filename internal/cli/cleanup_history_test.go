package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// validHistoryMessage is a message whose header parses.
const validHistoryMessage = "---json\n{\"schema\":1}\n---\nx\n"

// cleanup with a size policy brings each mailbox's history under the size it
// is given, oldest first, and leaves unread mail (unbounded mailboxes: 13.4k
// read, 14.7k sent and 13.5k receipt files in one mailbox, bead
// agent-message-queue-9df).
func TestCleanupHistoryMaxSizeRemovesOldestHistory(t *testing.T) {
	rootPath := newCleanupMailboxRoot(t, "claude")
	base := fsq.AgentBase(rootPath, "claude")
	files := map[string]string{
		"inbox/cur/2026-01-01T00-00-00.000Z_pid1_a.md":                   validHistoryMessage,
		"outbox/sent/2026-01-02T00-00-00.000Z_pid1_b.md":                 validHistoryMessage,
		"receipts/2026-01-03T00-00-00.000Z_pid1_c__claude__drained.json": "receipt1",
		"inbox/cur/2026-02-01T00-00-00.000Z_pid1_d.md":                   validHistoryMessage,
		"inbox/new/2025-01-01T00-00-00.000Z_pid1_e.md":                   "unread!!",
		"remote-files/" + strings.Repeat("f", 64) + ".png":               "an image",
	}
	for rel, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(base, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(base, "remote-files", strings.Repeat("f", 64)+".png"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", "30B", "--root", rootPath, "--yes"})
	}); err != nil {
		t.Fatalf("cleanup --history-max-size: %v", err)
	}
	for rel := range files {
		_, err := os.Lstat(filepath.Join(base, rel))
		kept := strings.Contains(rel, "2026-02-01") || strings.HasPrefix(rel, "inbox/new")
		if kept != (err == nil) {
			t.Fatalf("%s kept=%v, want %v", rel, err == nil, kept)
		}
	}
}

// History cleanup keeps a read review request nobody answered, so thread open
// still lists it (a 3-day-old open review request removed by
// --history-max-size 1B, review of bead agent-message-queue-9df).
func TestCleanupHistoryKeepsOpenObligations(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	ids, _ := writeThreadOpenMessages(t, base, []threadOpenMessage{{name: "rr", from: "alpha@s1", to: "beta@s1", kind: format.KindReviewRequest}}, func(int, threadOpenMessage) time.Time {
		return time.Now().UTC().Add(-3 * 24 * time.Hour)
	})
	s1 := filepath.Join(base, "s1")
	if err := os.Rename(filepath.Join(fsq.AgentInboxNew(s1, "beta"), ids["rr"]+".md"), filepath.Join(fsq.AgentInboxCur(s1, "beta"), ids["rr"]+".md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", "1B", "--root", s1, "--yes"})
	}); err != nil {
		t.Fatal(err)
	}
	if after := runThreadOpenJSON(t, base); len(after.Obligations) != 1 {
		t.Fatalf("open obligations after cleanup = %d, want 1", len(after.Obligations))
	}
}

// History cleanup also keeps the status reply that acknowledges an open
// question, in the same session and across sessions, so thread open still
// shows it acknowledged (an acknowledged question turned pending after
// --history-max-size 1B, review of bead agent-message-queue-9df).
func TestCleanupHistoryKeepsRepliesToOpenObligations(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	for _, session := range []string{"s1", "s2"} {
		base := filepath.Join(t.TempDir(), defaultCoopRoot)
		start := time.Now().UTC().Add(-3 * 24 * time.Hour)
		ids, _ := writeThreadOpenMessages(t, base, []threadOpenMessage{
			{name: "q", from: "alpha@s1", to: "beta@" + session, kind: format.KindQuestion},
			{name: "st", from: "beta@" + session, to: "alpha@s1", kind: format.KindStatus, refs: []string{"q"}},
		}, func(i int, _ threadOpenMessage) time.Time { return start.Add(time.Duration(i) * time.Hour) })
		s1, rs := filepath.Join(base, "s1"), filepath.Join(base, session)
		for _, move := range [][3]string{{rs, "beta", ids["q"]}, {s1, "alpha", ids["st"]}} {
			if err := os.Rename(filepath.Join(fsq.AgentInboxNew(move[0], move[1]), move[2]+".md"), filepath.Join(fsq.AgentInboxCur(move[0], move[1]), move[2]+".md")); err != nil {
				t.Fatal(err)
			}
		}
		for _, root := range []string{s1, rs} {
			if _, _, err := captureEnvOutput(t, func() error {
				return runCleanup([]string{"--history-max-size", "1B", "--root", root, "--yes"})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if after := runThreadOpenJSON(t, base); len(after.Obligations) != 1 || after.Obligations[0].State != "acknowledged" {
			t.Fatalf("%s: obligations after cleanup = %+v, want one acknowledged", session, after.Obligations)
		}
	}
}

// Review of the disk bounds, round 1, finding 1: history cleanup deleted a
// corrupt message, a dead-letter receipt and a fetched remote file that
// unread mail names.
func TestCleanupHistoryKeepsCorruptDeadLetterReceiptAndReferencedRemoteFile(t *testing.T) {
	rootPath := newCleanupMailboxRoot(t, "claude")
	base := fsq.AgentBase(rootPath, "claude")
	image := filepath.Join(base, "remote-files", strings.Repeat("f", 64)+".png")
	files := map[string]string{
		"inbox/new/2026-01-04T00-00-00.000Z_pid1_d.md":                   "---json\n{\"schema\":1}\n---\nsee\n\n[image: " + image + "]\n",
		"inbox/cur/2026-01-01T00-00-00.000Z_pid1_a.md":                   "not a message",
		"receipts/2026-01-02T00-00-00.000Z_pid1_b__claude__dlq.json":     "dlq receipt",
		"receipts/2026-01-03T00-00-00.000Z_pid1_c__claude__drained.json": "drained",
		"remote-files/" + strings.Repeat("f", 64) + ".png":               "an image",
	}
	for rel, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(base, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", "1B", "--history-older-than", "1h", "--root", rootPath, "--yes"})
	}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for rel := range files {
		_, err := os.Lstat(filepath.Join(base, rel))
		if kept := !strings.Contains(rel, "drained"); kept != (err == nil) {
			t.Fatalf("%s kept=%v, want %v", rel, err == nil, kept)
		}
	}
}

// Review of the disk bounds, round 1, finding 5: size cleanup deleted the
// answer of a question while a copy of the question stayed, so the question
// looked open again.
func TestCleanupHistoryKeepsAReplyWhileItsParentStays(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	start := time.Now().UTC().Add(-3 * 24 * time.Hour)
	ids, _ := writeThreadOpenMessages(t, base, []threadOpenMessage{
		{name: "q", from: "alpha@s1", to: "beta@s1,gamma@s1", kind: format.KindQuestion, thread: "work"},
		{name: "long", from: "beta@s1", to: "alpha@s1", kind: format.KindAnswer, thread: "work", refs: []string{"q"}},
		{name: "short", from: "gamma@s1", to: "alpha@s1", kind: format.KindAnswer, thread: "work", refs: []string{"q"}},
	}, func(i int, _ threadOpenMessage) time.Time { return start.Add(time.Duration(i) * time.Hour) })
	s1 := filepath.Join(base, "s1")
	for _, move := range [][2]string{{"beta", "q"}, {"gamma", "q"}, {"alpha", "long"}, {"alpha", "short"}} {
		if err := os.Rename(filepath.Join(fsq.AgentInboxNew(s1, move[0]), ids[move[1]]+".md"), filepath.Join(fsq.AgentInboxCur(s1, move[0]), ids[move[1]]+".md")); err != nil {
			t.Fatal(err)
		}
	}
	// beta's long answer pushes alpha's and beta's mailboxes past the bound;
	// gamma's mailbox, which keeps a copy of the question, stays under it.
	for _, path := range []string{filepath.Join(fsq.AgentOutboxSent(s1, "beta"), ids["long"]+".md"), filepath.Join(fsq.AgentInboxCur(s1, "alpha"), ids["long"]+".md")} {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(strings.Repeat("x", 8192))
		_ = f.Close()
	}
	if after := runThreadOpenJSON(t, base); len(after.Obligations) != 0 {
		t.Fatalf("obligations before cleanup = %+v, want none", after.Obligations)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", "4KB", "--root", s1, "--yes"})
	}); err != nil {
		t.Fatal(err)
	}
	if after := runThreadOpenJSON(t, base); len(after.Obligations) != 0 {
		t.Fatalf("obligations after cleanup = %+v, want none", after.Obligations)
	}
}

// Review of the disk bounds, round 2, finding 1: keeping replies whose
// parent stays was transitive, so a chain of plain replies under one kept
// copy of its first message outgrew the size bound.
func TestCleanupHistoryCutsAPlainReplyChainToTheBound(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	start := time.Now().UTC().Add(-3 * 24 * time.Hour)
	chain := []threadOpenMessage{{name: "m0", from: "alpha@s1", to: "beta@s1,gamma@s1", kind: format.KindStatus, thread: "chain"}}
	for i := 1; i < 10; i++ {
		from, to := "beta@s1", "alpha@s1"
		if i%2 == 0 {
			from, to = to, from
		}
		chain = append(chain, threadOpenMessage{name: fmt.Sprintf("m%d", i), from: from, to: to, kind: format.KindStatus, thread: "chain", refs: []string{fmt.Sprintf("m%d", i-1)}})
	}
	ids, _ := writeThreadOpenMessages(t, base, chain, func(i int, _ threadOpenMessage) time.Time { return start.Add(time.Duration(i) * time.Hour) })
	s1 := filepath.Join(base, "s1")
	for _, agent := range []string{"alpha", "beta", "gamma"} {
		for _, id := range ids {
			from := filepath.Join(fsq.AgentInboxNew(s1, agent), id+".md")
			if _, err := os.Stat(from); err == nil {
				if err := os.Rename(from, filepath.Join(fsq.AgentInboxCur(s1, agent), id+".md")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	const bound = 2048
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", fmt.Sprintf("%dB", bound), "--root", s1, "--yes"})
	}); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, dir := range []string{fsq.AgentInboxCur(s1, "alpha"), fsq.AgentOutboxSent(s1, "alpha")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	if total > bound {
		t.Fatalf("alpha keeps %d bytes of a plain reply chain, want at most %d", total, bound)
	}
}

// Review of the disk bounds, round 3, finding 1: size cleanup kept the
// closing answer but removed the status between it and the question, so the
// answer's lineage broke and the question looked open again.
func TestCleanupHistoryKeepsThePathToAClosingAnswer(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	base := filepath.Join(t.TempDir(), defaultCoopRoot)
	start := time.Now().UTC().Add(-3 * 24 * time.Hour)
	ids, _ := writeThreadOpenMessages(t, base, []threadOpenMessage{
		{name: "q", from: "alpha@s1", to: "beta@s1,delta@s1", kind: format.KindQuestion, thread: "work"},
		{name: "d", from: "delta@s1", to: "alpha@s1", kind: format.KindAnswer, thread: "work", refs: []string{"q"}},
		{name: "s", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, thread: "work", refs: []string{"q"}},
		{name: "r", from: "beta@s1", to: "alpha@s1", kind: format.KindAnswer, thread: "work", refs: []string{"s"}},
	}, func(i int, _ threadOpenMessage) time.Time { return start.Add(time.Duration(i) * time.Hour) })
	s1 := filepath.Join(base, "s1")
	for _, move := range [][2]string{{"beta", "q"}, {"delta", "q"}, {"alpha", "d"}, {"alpha", "s"}, {"alpha", "r"}} {
		if err := os.Rename(filepath.Join(fsq.AgentInboxNew(s1, move[0]), ids[move[1]]+".md"), filepath.Join(fsq.AgentInboxCur(s1, move[0]), ids[move[1]]+".md")); err != nil {
			t.Fatal(err)
		}
	}
	// The long answer pushes alpha's and beta's mailboxes past a bound that
	// delta's mailbox, which keeps the question, fits.
	for _, path := range []string{filepath.Join(fsq.AgentOutboxSent(s1, "beta"), ids["r"]+".md"), filepath.Join(fsq.AgentInboxCur(s1, "alpha"), ids["r"]+".md")} {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(strings.Repeat("x", 8192))
		_ = f.Close()
	}
	var bound int64
	for _, path := range []string{filepath.Join(fsq.AgentInboxCur(s1, "delta"), ids["q"]+".md"), filepath.Join(fsq.AgentOutboxSent(s1, "delta"), ids["d"]+".md")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		bound += info.Size()
	}
	if after := runThreadOpenJSON(t, base); len(after.Obligations) != 0 {
		t.Fatalf("obligations before cleanup = %+v, want none", after.Obligations)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--history-max-size", fmt.Sprintf("%dB", bound), "--root", s1, "--yes"})
	}); err != nil {
		t.Fatal(err)
	}
	if after := runThreadOpenJSON(t, base); len(after.Obligations) != 0 {
		t.Fatalf("obligations after cleanup = %+v, want none", after.Obligations)
	}
}
