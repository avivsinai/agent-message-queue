package acp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// A Claude turn is posted when its first assistant line arrives, shown as
// running, edited at most once every mirrorEditGap while it runs, and edited
// a last time with its answer. A tick that changes nothing sends nothing.
func TestMirrorTickShowsARunningTurnLive(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	transcript := filepath.Join(canonicalTempDir(t), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","uuid":"o0","promptId":"p0","timestamp":"2026-11-04T09:00:00Z","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendLines := func(lines ...string) {
		t.Helper()
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	var sent []string
	savedPost, savedEdit := postAnswer, editMessage
	t.Cleanup(func() { postAnswer, editMessage = savedPost, savedEdit })
	postAnswer = func(_, content string, _ time.Duration) (string, error) {
		sent = append(sent, "post: "+content)
		return strings.Repeat("a", 64), nil
	}
	editMessage = func(_, content string, _ time.Duration) error {
		sent = append(sent, "edit: "+content)
		return nil
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha", Harness: "claude", Transcript: transcript}
	s := NewServer(Config{RemoteBinding: true, Machine: true, StateDir: canonicalTempDir(t)}, "test")
	target := dialogTarget{name: "alpha", channel: "6eff60e4-32ab-48ec-bd3d-f4c97872f370"}
	now := time.Now()
	s.mirrorBinding(target, b, now, false) // first sight: starts at the end

	steps := []struct {
		lines []string
		after time.Duration
		want  []string
	}{{
		lines: []string{
			`{"type":"user","uuid":"o1","promptId":"p1","timestamp":"2026-11-04T09:01:00Z","message":{"content":"run the tests"}}`,
			`{"type":"assistant","uuid":"a1","timestamp":"2026-11-04T09:01:02Z","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		},
		want: []string{"post: ▶ run the tests\n… running · 1 tool (Bash) · 2s"},
	}, {
		lines: []string{`{"type":"assistant","uuid":"a2","timestamp":"2026-11-04T09:01:04Z","message":{"id":"m2","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t2","name":"Read","input":{}}]}}`},
		after: time.Second,
	}, {
		after: 3 * time.Second,
		want:  []string{"edit: ▶ run the tests\n… running · 2 tools (Bash, Read) · 4s"},
	}, {
		lines: []string{`{"type":"assistant","uuid":"a3","timestamp":"2026-11-04T09:01:06Z","message":{"id":"m3","stop_reason":"end_turn","content":[{"type":"text","text":"All pass."}]}}`},
		after: 6 * time.Second,
		want:  []string{"edit: ▶ run the tests\n✓ 2 tools (Bash, Read) · 6s\nAll pass."},
	}, {
		after: 9 * time.Second,
	}}
	for i, step := range steps {
		if step.lines != nil {
			appendLines(step.lines...)
		}
		sent = nil
		s.mirrorBinding(target, b, now.Add(step.after), false)
		if !slices.Equal(sent, step.want) {
			t.Fatalf("step %d at +%s: sent %q; want %q", i+1, step.after, sent, step.want)
		}
	}
}

// agent-message-queue-6r9 review: a /resume to an older session after the
// mirror first saw an empty transcript posted that session's old turns.
func TestMirrorResumeToOlderSessionPostsNoOldTurn(t *testing.T) {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	cfg := canonicalTempDir(t)
	proj := filepath.Join(cfg, "projects", "-work-proj")
	for _, d := range []string{proj, filepath.Join(cfg, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := `{"type":"user","uuid":"h1","promptId":"hp1","timestamp":"2026-11-04T08:00:00Z","origin":{"kind":"human"},"message":{"content":"old prompt"}}` + "\n" +
		`{"type":"assistant","uuid":"ha1","timestamp":"2026-11-04T08:00:02Z","message":{"id":"hm1","stop_reason":"end_turn","content":[{"type":"text","text":"old answer"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "old.jsonl"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "cur.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	session := func(id string) {
		entry := `{"pid":` + strconv.Itoa(pid) + `,"sessionId":"` + id + `","cwd":"/work/proj"}`
		if err := os.WriteFile(filepath.Join(cfg, "sessions", strconv.Itoa(pid)+".json"), []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var posts []string
	savedPost := postAnswer
	t.Cleanup(func() { postAnswer = savedPost })
	postAnswer = func(_, content string, _ time.Duration) (string, error) {
		posts = append(posts, content)
		return strings.Repeat("a", 64), nil
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha", Harness: "claude", Transcript: filepath.Join(proj, "cur.jsonl")}
	b.Holder.PID = pid
	s := NewServer(Config{RemoteBinding: true, Machine: true, StateDir: canonicalTempDir(t)}, "test")
	target := dialogTarget{name: "alpha", channel: "6eff60e4-32ab-48ec-bd3d-f4c97872f370"}
	now := time.Now()

	session("cur")
	s.mirrorBinding(target, b, now, false)
	session("old")
	s.mirrorBinding(target, b, now.Add(time.Minute), false)
	if len(posts) != 0 {
		t.Fatalf("posts = %q; want none: the resumed session's turns are from before first sight", posts)
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
