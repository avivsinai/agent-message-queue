package acp

import (
	"errors"
	"fmt"
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
// running, edited only when what it shows changed and at most once every
// mirrorEditGap, and edited at once with its answer. A tick that changes
// nothing sends nothing. The second turn's tick crashes after its post: the
// post still gets its final edit (agent-message-queue-07h review). The third
// turn changes 12 times and gets mirrorProgressMax progress edits.
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
	// Lines at sec seconds after 09:01:00.
	ts := func(sec int) string {
		return time.Date(2026, 11, 4, 9, 1, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
	}
	opener := func(id string, sec int, prompt string) string {
		return `{"type":"user","uuid":"` + id + `","promptId":"p` + id + `","timestamp":"` + ts(sec) + `","message":{"content":"` + prompt + `"}}`
	}
	assistant := func(id string, sec int, stop, block string) string {
		return `{"type":"assistant","uuid":"a` + id + `","timestamp":"` + ts(sec) + `","message":{"id":"` + id + `","stop_reason":"` + stop + `","content":[` + block + `]}}`
	}
	toolUse := func(id string, sec int, name string) string {
		return assistant(id, sec, "tool_use", `{"type":"tool_use","id":"t`+id+`","name":"`+name+`","input":{}}`)
	}
	say := func(id string, sec int, stop, text string) string {
		return assistant(id, sec, stop, `{"type":"text","text":"`+text+`"}`)
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

	dir, _ := mirror.DirFor("alpha")
	type step struct {
		lines []string
		after int  // tick time, seconds after now
		crash bool // state.json is lost after the post
		want  []string
	}
	steps := []step{
		{lines: []string{opener("o1", 0, "run the tests"), toolUse("m1", 2, "Bash")}, after: 0,
			want: []string{"post: ▶ run the tests\n… running · 1 tool (Bash) · 2s"}},
		{lines: []string{toolUse("m2", 10, "Read")}, after: 10},
		{lines: []string{toolUse("m3", 35, "Grep")}, after: 35,
			want: []string{"edit: ▶ run the tests\n… running · 3 tools (Bash, Read, Grep) · 35s"}},
		{lines: []string{say("m4", 40, "tool_use", "Still checking.")}, after: 40},
		{after: 70},
		{lines: []string{say("m5", 72, "end_turn", "All pass.")}, after: 72,
			want: []string{"edit: ▶ run the tests\n✓ 3 tools (Bash, Read, Grep) · 1m12s\nAll pass."}},
		{after: 75},
		{lines: []string{opener("o2", 100, "commit"), toolUse("n1", 101, "Bash")}, after: 100, crash: true,
			want: []string{"post: ▶ commit\n… running · 1 tool (Bash) · 1s"}},
		{lines: []string{say("n2", 103, "end_turn", "Committed.")}, after: 103,
			want: []string{"edit: ▶ commit\n✓ 1 tool (Bash) · 3s\nCommitted."}},
		{lines: []string{opener("o3", 200, "loop"), toolUse("k0", 201, "Bash")}, after: 200,
			want: []string{"post: ▶ loop\n… running · 1 tool (Bash) · 1s"}},
	}
	for i := 1; i <= 12; i++ {
		st := step{lines: []string{toolUse("k"+strconv.Itoa(i), 201+31*i, "Bash")}, after: 200 + 31*i}
		if i <= mirrorProgressMax {
			st.want = []string{fmt.Sprintf("edit: ▶ loop\n… running · %d tools (Bash ×%d) · %s", i+1, i+1, time.Duration(1+31*i)*time.Second)}
		}
		steps = append(steps, st)
	}
	steps = append(steps, step{lines: []string{say("k13", 574, "end_turn", "Done.")}, after: 574,
		want: []string{"edit: ▶ loop\n✓ 13 tools (Bash ×13) · 6m14s\nDone."}})
	for i, step := range steps {
		if step.lines != nil {
			appendLines(step.lines...)
		}
		sent = nil
		saved, _, _ := readMirrorFile(dir)
		s.mirrorBinding(target, b, now.Add(time.Duration(step.after)*time.Second), false)
		if step.crash {
			if err := writeMirrorFile(dir, saved); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(sent, step.want) {
			t.Fatalf("step %d at +%ds: sent %q; want %q", i+1, step.after, sent, step.want)
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
