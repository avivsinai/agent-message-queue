package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
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
// turn changes 12 times and gets mirrorProgressMax progress edits, then
// still shows the interrupt (review: the cap kept it "running"). The fourth
// turn is continued by a Stop hook 20 times in 60 s and is edited once per
// gap, ending with its last answer (review: 21 stored events).
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
	interrupt := `{"type":"user","uuid":"i3","promptId":"po3","timestamp":"` + ts(574) + `","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
	steps = append(steps,
		step{lines: []string{interrupt}, after: 574,
			want: []string{"edit: ▶ loop\n✗ interrupted · 13 tools (Bash ×13) · 6m14s"}},
		step{lines: []string{say("k13", 580, "end_turn", "Done.")}, after: 580,
			want: []string{"edit: ▶ loop\n✓ 13 tools (Bash ×13) · 6m20s\nDone."}},
		step{lines: []string{opener("o4", 700, "ship"), say("h0", 701, "end_turn", "done 0")}, after: 701,
			want: []string{"post: ▶ ship\n✓ 1s\ndone 0"}})
	for i := 1; i <= 20; i++ {
		hook := `{"type":"user","uuid":"g` + strconv.Itoa(i) + `","promptId":"po4","isMeta":true,"timestamp":"` + ts(700+3*i) + `","message":{"content":[{"type":"text","text":"Stop hook feedback: keep going"}]}}`
		st := step{lines: []string{hook, say("h"+strconv.Itoa(i), 701+3*i, "end_turn", "done "+strconv.Itoa(i))}, after: 702 + 3*i}
		if i%10 == 0 {
			st.want = []string{fmt.Sprintf("edit: ▶ ship\n✓ %s\ndone %d", time.Duration(1+3*i)*time.Second, i)}
		}
		steps = append(steps, st)
	}
	// Review of the mirror: a late line of an ended turn edited its post
	// only to show a longer elapsed time.
	steps = append(steps, step{after: 800},
		step{lines: []string{assistant("h20", 790, "end_turn", `{"type":"thinking","thinking":"…"}`)}, after: 840})
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

// mirrorFixture is a mailbox binding whose transcript the test appends to,
// mirrored into a channel by a server that records each post and edit.
type mirrorFixture struct {
	t          *testing.T
	s          *Server
	b          binding.Binding
	target     dialogTarget
	transcript string
	now        time.Time
	sent       []string
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	f := &mirrorFixture{t: t, transcript: filepath.Join(canonicalTempDir(t), "session.jsonl"), now: time.Now()}
	if err := os.WriteFile(f.transcript, []byte(`{"type":"user","uuid":"o0","promptId":"p0","timestamp":"2026-11-04T09:00:00Z","message":{"content":"earlier"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	savedPost, savedEdit := postAnswer, editMessage
	t.Cleanup(func() { postAnswer, editMessage = savedPost, savedEdit })
	postAnswer = func(_, content string, _ time.Duration) (string, error) {
		f.sent = append(f.sent, "post: "+content)
		return strings.Repeat("a", 64), nil
	}
	editMessage = func(_, content string, _ time.Duration) error {
		f.sent = append(f.sent, "edit: "+content)
		return nil
	}
	f.b = binding.Binding{Carrier: binding.CarrierMailbox, Root: canonicalTempDir(t), Handle: "agent", Name: "alpha", Harness: "claude", Transcript: f.transcript}
	f.s = NewServer(Config{RemoteBinding: true, Machine: true, StateDir: canonicalTempDir(t)}, "test")
	f.target = dialogTarget{name: "alpha", channel: "6eff60e4-32ab-48ec-bd3d-f4c97872f370"}
	f.s.mirrorBinding(f.target, f.b, f.now, false) // first sight: starts at the end
	return f
}

// ts is sec seconds after 09:01:00.
func (f *mirrorFixture) ts(sec float64) string {
	return time.Date(2026, 11, 4, 9, 1, 0, 0, time.UTC).Add(time.Duration(sec * float64(time.Second))).Format(time.RFC3339Nano)
}

// add appends transcript lines, each given as its top-level fields.
func (f *mirrorFixture) add(lines ...map[string]any) {
	f.t.Helper()
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			f.t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	file, err := os.OpenFile(f.transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	_, err = file.WriteString(b.String())
	_ = file.Close()
	if err != nil {
		f.t.Fatal(err)
	}
}

// tick runs one mirror tick after seconds and returns what it sent.
func (f *mirrorFixture) tick(after int) []string {
	f.sent = nil
	f.s.mirrorBinding(f.target, f.b, f.now.Add(time.Duration(after)*time.Second), false)
	return f.sent
}

func (f *mirrorFixture) userLine(uuid, promptID string, sec float64, text string, extra map[string]any) map[string]any {
	l := map[string]any{"type": "user", "uuid": uuid, "promptId": promptID, "timestamp": f.ts(sec), "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func (f *mirrorFixture) assistantLine(id string, sec float64, stop string, block map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "uuid": "a" + id, "timestamp": f.ts(sec), "message": map[string]any{"id": id, "stop_reason": stop, "content": []any{block}}}
}

func toolBlock(name string) map[string]any {
	return map[string]any{"type": "tool_use", "id": "t" + name, "name": name, "input": map[string]any{}}
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

// Regression (idle profile of a machine agent): every 2 s tick opened and
// read each live session's transcript even when it had not changed. An
// unchanged transcript is not read again; a grown one is.
func TestMirrorReadsATranscriptOnlyWhenItChanged(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file mode that denies its owner a read")
	}
	f := newMirrorFixture(t)
	if got := f.tick(1); len(got) != 0 {
		t.Fatalf("sent %q; want nothing", got)
	}
	// A read now fails and posts the unavailable note, so a tick that
	// sends nothing read nothing. The mode change keeps size and mtime.
	if err := os.Chmod(f.transcript, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.tick(3); len(got) != 0 {
		t.Fatalf("the unchanged transcript was read again: sent %q", got)
	}
	if err := os.Chmod(f.transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m1", 1, "end_turn", textBlock("Shipped.")))
	if got, want := f.tick(5), []string{"post: ▶ ship\n✓ 1s\nShipped."}; !slices.Equal(got, want) {
		t.Fatalf("sent %q; want %q", got, want)
	}
}

// Review of the mirror: a turn a Stop hook continued was no longer the live
// turn, so its post never showed it running again.
func TestMirrorReopenedTurnShowsRunning(t *testing.T) {
	f := newMirrorFixture(t)
	f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m1", 1, "end_turn", textBlock("Shipped.")))
	if got, want := f.tick(1), []string{"post: ▶ ship\n✓ 1s\nShipped."}; !slices.Equal(got, want) {
		t.Fatalf("sent %q; want %q", got, want)
	}
	f.add(f.userLine("g1", "p1", 2, "Stop hook feedback: run the tests", map[string]any{"isMeta": true}), f.assistantLine("m2", 3, "tool_use", toolBlock("Bash")))
	if got, want := f.tick(40), []string{"edit: ▶ ship\n… running · 1 tool (Bash) · 3s"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q; want %q", got, want)
	}
}

// Review of the mirror: a Stop hook that loops, its tool line and its
// answer read in separate ticks, edited the post twice per cycle without
// end. Past its counted edits, the post is edited at most once per gap.
func TestMirrorLoopingStopHookEditsOncePerGap(t *testing.T) {
	f := newMirrorFixture(t)
	f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m0", 0, "end_turn", textBlock("done 0")))
	f.tick(0)
	var at []int
	for i := 1; i <= 12; i++ {
		sec := 40 * i
		f.add(f.userLine("g"+strconv.Itoa(i), "p1", float64(sec), "Stop hook feedback: keep going", map[string]any{"isMeta": true}),
			f.assistantLine("t"+strconv.Itoa(i), float64(sec), "tool_use", toolBlock("Bash")))
		for range f.tick(sec) {
			at = append(at, sec)
		}
		f.add(f.assistantLine("m"+strconv.Itoa(i), float64(sec+2), "end_turn", textBlock("done "+strconv.Itoa(i))))
		for range f.tick(sec + 2) {
			at = append(at, sec+2)
		}
	}
	if len(at) <= mirrorProgressMax {
		t.Fatalf("edits at +%ds; want the cycles shown", at)
	}
	for i := mirrorProgressMax + 1; i < len(at); i++ {
		if time.Duration(at[i]-at[i-1])*time.Second < mirrorEditGap {
			t.Fatalf("edits at +%ds; want them at least %s apart", at, mirrorEditGap)
		}
	}
}

// Review of the mirror: past the edit cap a Continued edit was dropped, so
// a turn a Stop hook kept going lost its last answer.
func TestMirrorLastAnswerLandsPastTheCap(t *testing.T) {
	f := newMirrorFixture(t)
	f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m0", 0, "end_turn", textBlock("done 0")))
	f.tick(0)
	var last string
	for i := 1; i <= 14; i++ {
		sec := 31 * i
		f.add(f.userLine("g"+strconv.Itoa(i), "p1", float64(sec), "Stop hook feedback: keep going", map[string]any{"isMeta": true}),
			f.assistantLine("m"+strconv.Itoa(i), float64(sec), "end_turn", textBlock("done "+strconv.Itoa(i))))
		if sent := f.tick(sec); len(sent) > 0 {
			last = sent[len(sent)-1]
		}
	}
	for sec := 31 * 15; sec <= 31*14+int(mirrorEditGapMax/time.Second); sec += 30 {
		if sent := f.tick(sec); len(sent) > 0 {
			last = sent[len(sent)-1]
		}
	}
	if !strings.HasSuffix(last, "\ndone 14") {
		t.Fatalf("last edit %q; want the last answer, done 14", last)
	}
}

// Review of the mirror: a looping Stop hook past the cap left its turn
// live while its post showed an answer, and turning the mirror off then
// erased that answer. Off ends a running post without an answer and leaves
// an ended post's answer.
func TestMirrorOffKeepsTheShownAnswer(t *testing.T) {
	t.Run("ended post", func(t *testing.T) {
		f := newMirrorFixture(t)
		f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m0", 0, "end_turn", textBlock("done 0")))
		f.tick(0)
		var sec int
		for i := 1; i <= 13; i++ {
			sec = 40 * i
			f.add(f.userLine("g"+strconv.Itoa(i), "p1", float64(sec), "Stop hook feedback: keep going", map[string]any{"isMeta": true}),
				f.assistantLine("t"+strconv.Itoa(i), float64(sec), "tool_use", toolBlock("Bash")))
			f.tick(sec)
			f.add(f.assistantLine("m"+strconv.Itoa(i), float64(sec+2), "end_turn", textBlock("done "+strconv.Itoa(i))))
			f.tick(sec + 2)
		}
		dir, _ := mirror.DirFor("alpha")
		if st, _, _ := readMirrorFile(dir); !slices.ContainsFunc(st.Queue, func(ev mirror.Event) bool { return ev.Kind == mirror.Continued }) {
			t.Fatalf("queue %+v; want a Continued waiting", st.Queue)
		}
		if err := mirror.SetOff("alpha", true); err != nil {
			t.Fatal(err)
		}
		for after := sec + 4; after <= sec+int(mirrorEditGapMax/time.Second); after += 30 {
			if sent := f.tick(after); len(sent) > 0 {
				t.Fatalf("at +%ds sent %q; want the post to keep its answer", after, sent)
			}
		}
	})
	t.Run("running post", func(t *testing.T) {
		f := newMirrorFixture(t)
		f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m1", 1, "tool_use", toolBlock("Bash")))
		f.tick(1)
		if err := mirror.SetOff("alpha", true); err != nil {
			t.Fatal(err)
		}
		if got, want := f.tick(2), []string{"edit: ▶ ship\n■ ended (mirror off) · 1 tool (Bash) · 1s"}; !slices.Equal(got, want) {
			t.Fatalf("sent %q; want %q", got, want)
		}
		f.add(f.assistantLine("m2", 3, "end_turn", textBlock("Shipped.")))
		if got := f.tick(60); len(got) > 0 {
			t.Fatalf("sent %q after the off edit; want nothing", got)
		}
	})
}

// Review of the mirror: the queue kept every waiting Continued, so turns
// continued faster than their gap grew it without bound. It stays within
// mirrorQueueMax, and each post ends with its newest answer or an earlier
// one, never without one.
func TestMirrorQueueBoundsWaitingContinued(t *testing.T) {
	f := newMirrorFixture(t)
	dir, _ := mirror.DirFor("alpha")
	const turns = 12
	for i := 1; i <= turns; i++ {
		n, sec := strconv.Itoa(i), 5*i
		f.add(f.userLine("o"+n, "p"+n, float64(sec), "task "+n, nil), f.assistantLine("m"+n, float64(sec+1), "end_turn", textBlock("a "+n)))
		f.tick(sec + 1)
		f.add(f.assistantLine("m"+n, float64(sec+2), "end_turn", textBlock("b "+n)))
		f.tick(sec + 2)
		if st, _, _ := readMirrorFile(dir); len(st.Queue) > mirrorQueueMax {
			t.Fatalf("after turn %d the queue holds %d events; want at most %d", i, len(st.Queue), mirrorQueueMax)
		}
	}
	for sec := 5*turns + 30; sec <= 5*turns+120; sec += 30 {
		f.tick(sec)
	}
	for i := 1; i <= turns; i++ {
		n := strconv.Itoa(i)
		c, _ := readMirrorClaim(filepath.Join(dir, "posted", "o"+n+".json"))
		if !strings.HasSuffix(c.Text, "\na "+n) && !strings.HasSuffix(c.Text, "\na "+n+"\nb "+n) {
			t.Fatalf("post of turn %d shows %q; want its answer", i, c.Text)
		}
	}
}

// Review of the mirror: a running post's Ended was dropped by the queue
// cap when six turns arrived after a backoff, so the post showed its turn
// running for ever.
func TestMirrorRunningPostEndsAfterABacklog(t *testing.T) {
	f := newMirrorFixture(t)
	f.add(f.userLine("o1", "p1", 0, "ship", nil), f.assistantLine("m1", 1, "tool_use", toolBlock("Bash")))
	f.tick(1)
	edit := editMessage
	editMessage = func(string, string, time.Duration) error { return &notSentError{errors.New("relay down")} }
	f.add(f.assistantLine("m2", 35, "tool_use", toolBlock("Read")))
	f.tick(35) // the progress edit is not sent: the binding backs off
	editMessage = edit
	f.add(f.assistantLine("m3", 40, "end_turn", textBlock("Shipped.")))
	for i := 2; i <= 7; i++ {
		n := strconv.Itoa(i)
		f.add(f.userLine("o"+n, "p"+n, float64(40+2*i), "task "+n, nil), f.assistantLine("m"+n+"x", float64(41+2*i), "end_turn", textBlock("done "+n)))
	}
	f.tick(35 + int(mirrorBackoffMin/time.Second) + 1)
	dir, _ := mirror.DirFor("alpha")
	c, _ := readMirrorClaim(filepath.Join(dir, "posted", "o1.json"))
	if c.Running || !strings.HasSuffix(c.Text, "\nShipped.") {
		t.Fatalf("post of the running turn: running %t, %q; want it ended with its answer", c.Running, c.Text)
	}
}

// A doorbell turn whose reply to a Buzz message reached the buzz inbox
// during the turn shows Buzz as its cause and omits the answer the carrier
// posts itself.
func TestMirrorDoorbellTurnThatRepliedToBuzzShowsBuzz(t *testing.T) {
	f := newMirrorFixture(t)
	write := func(dir string, m format.Message) {
		t.Helper()
		data, err := m.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, m.Header.ID+".md"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(fsq.AgentInboxCur(f.b.Root, "agent"), format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: "prompt-1", From: mailboxSender, To: []string{"agent"}, Thread: "buzz-1", Created: f.ts(-5)}, Body: "deploy"})
	write(fsq.AgentInboxNew(f.b.Root, mailboxSender), format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: "reply-1", From: "agent", To: []string{mailboxSender}, Thread: "buzz-1", Created: f.ts(1.5), Refs: []string{"prompt-1"}}, Body: "Deployed."})
	f.add(f.userLine("o1", "p1", 0, harness.Doorbell, nil), f.assistantLine("m1", 1, "tool_use", toolBlock("Bash")), f.assistantLine("m2", 2, "end_turn", textBlock("Replied.")))
	if got, want := f.tick(2), []string{"post: ▶ Buzz\n✓ 1 tool (Bash) · 2s"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q; want %q", got, want)
	}
}

// Review of the mirror: the "earlier turns" note of a backlog's turn was
// lost at the post's next edit.
func TestMirrorEditKeepsEarlierNote(t *testing.T) {
	f := newMirrorFixture(t)
	claim := filepath.Join(canonicalTempDir(t), "o1.json")
	turn := mirror.Turn{ID: "o1", Cause: "ship", End: mirror.EndDone, Text: "Shipped.", Earlier: 3}
	if _, err := f.s.mirrorPost(claim, f.target, f.b, turn, f.now); err != nil {
		t.Fatal(err)
	}
	turn.Earlier, turn.Text = 0, "Shipped and tagged."
	if _, err := f.s.mirrorEdit(claim, f.target, f.b, mirror.Event{Kind: mirror.Continued, Turn: turn}, f.now.Add(mirrorEditGap)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"post: ▶ ship\n✓\nShipped.\n(+3 earlier turns not shown)", "edit: ▶ ship\n✓\nShipped and tagged.\n(+3 earlier turns not shown)"}; !slices.Equal(f.sent, want) {
		t.Fatalf("sent %q; want %q", f.sent, want)
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
