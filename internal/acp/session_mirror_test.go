package acp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// The mirror posts a finished turn once. A post proved not sent keeps the
// turn queued while the cursor moves past its lines, the next tick after the
// backoff posts it, and a tick over the same file posts nothing more.
func TestMirrorTickPostsEachTurnOnce(t *testing.T) {
	const channel = "6eff60e4-32ab-48ec-bd3d-f4c97872f370"
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	transcript := filepath.Join(canonicalTempDir(t), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","uuid":"o0","promptId":"p0","timestamp":"2026-11-04T09:00:00Z","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha", Harness: "claude", Transcript: transcript}
	var posts []string
	sendErr := error(&notSentError{errors.New("no buzz CLI")})
	savedPost := postAnswer
	t.Cleanup(func() { postAnswer = savedPost })
	postAnswer = func(ch, content string, _ time.Duration) (string, error) {
		posts = append(posts, ch+": "+content)
		if sendErr != nil {
			return "", sendErr
		}
		return strings.Repeat("a", 64), nil
	}
	s := NewServer(Config{RemoteBinding: true, Machine: true, StateDir: canonicalTempDir(t)}, "test")
	target := dialogTarget{name: "alpha", channel: channel}
	now := time.Now()

	s.mirrorBinding(target, b, now, false) // first sight: starts at the end
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"type":"user","uuid":"o1","promptId":"p1","timestamp":"2026-11-04T09:01:00Z","message":{"content":"run the tests"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","timestamp":"2026-11-04T09:01:02Z","message":{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"All pass."}]}}` + "\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	size := fileSize(t, transcript)

	s.mirrorBinding(target, b, now, false)
	dir, _ := mirror.DirFor("alpha")
	st, _, _ := readMirrorFile(dir)
	if len(posts) != 1 || len(st.Queue) != 1 || st.Offset != size {
		t.Fatalf("after a post not sent: posts %q, queue %d, offset %d of %d; want one try, the turn queued, the offset at the end", posts, len(st.Queue), st.Offset, size)
	}
	if _, err := os.Lstat(filepath.Join(dir, "posted", "o1.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim of a post not sent: %v; want it removed", err)
	}

	sendErr = nil
	later := now.Add(mirrorBackoffMin + time.Second)
	s.mirrorBinding(target, b, later, false)
	s.mirrorBinding(target, b, later.Add(mirrorInterval), false)
	want := channel + ": ▶ run the tests\n✓ 2s\nAll pass."
	if !slices.Equal(posts[1:], []string{want}) {
		t.Fatalf("posts = %q; want the turn posted once: %q", posts[1:], want)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
