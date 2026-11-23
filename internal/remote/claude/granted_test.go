package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

const grantedHost = "link-aaaaaaaaaaaaaaaa"

// setRegistry rewrites the session registry entry of pid with changes, by a
// rename, as the harness replaces it.
func setRegistry(t *testing.T, home string, pid int, changes map[string]any) {
	t.Helper()
	path := filepath.Join(registryDir(defaultConfigDir(home)), fmt.Sprintf("%d.json", pid))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reg := map[string]any{}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	for k, v := range changes {
		reg[k] = v
	}
	if raw, err = json.Marshal(reg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// grantedTarget is a fake target whose session runs in this process and is
// idle, so the session id is live.
func grantedTarget(t *testing.T) (*fakeTarget, int) {
	t.Helper()
	pid := os.Getpid()
	ft := newFakeTarget(t, pid, nil)
	setRegistry(t, ft.home, pid, map[string]any{"status": "idle"})
	return ft, pid
}

// grantedAttach attaches to ft's session with the endpoint state directory
// stateDir and drops the attachment's poller at the end of the test.
func grantedAttach(t *testing.T, ft *fakeTarget, pid int, stateDir string) *Attachment {
	t.Helper()
	att, err := Attach(config{Pid: pid, Home: ft.home, Target: "cc-1"}, WithStateDir(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	return att
}

func grantedKey(id string) requests.Key {
	return requests.Key{CreatorHost: grantedHost, TargetID: "cc-1", RequestID: id}
}

func grantedRequest(id, text string) core.BoundRequest {
	return core.BoundRequest{
		Key: grantedKey(id), Epoch: SentinelUnpinned, NotAfter: "2099-01-01T00:00:00Z",
		Input:         protocol.SubmitInput{Text: text, Deliver: protocol.DeliverTurn, Busy: protocol.BusyReject},
		NativeSession: "sess-abc", Granted: true,
	}
}

// askingReport is the prompt report of the session in default mode.
var askingReport = core.Prompt{NativeSession: "sess-abc", Mode: "default"}

// hookContextLine is the transcript entry Claude Code 2.1.330 writes for a
// UserPromptSubmit hook's added context.
func hookContextLine(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":      "attachment",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"attachment": map[string]any{
			"type": "hook_additional_context", "hookName": "UserPromptSubmit", "hookEvent": "UserPromptSubmit",
			"toolUseID": "toolu_1", "content": []string{content},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stubEnvelope is the envelope of the granted stub of msgID, as a submit
// writes it.
func stubEnvelope(t *testing.T, msgID string) string {
	t.Helper()
	f, err := BuildFrame("amq-cc-1", grantedStub(msgID))
	if err != nil {
		t.Fatal(err)
	}
	return f.Message.Content
}

// closedTurn is a prompt and its answer: the transcript a session holds
// before the stub arrives.
func closedTurn(t *testing.T) []string {
	return []string{transcriptLine(t, "user", "an earlier prompt"), transcriptLine(t, "assistant", "an earlier answer")}
}

func sha(text string) string { return sha256Hex([]byte(text)) }

// imageLine is a prompt of an image alone: a user entry whose content is one
// image block. No recorded transcript here holds an image entry, so the
// block follows the documented message content block type (an image with a
// base64 source).
func imageLine(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":      "user",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hugePromptLine is a prompt longer than the reader's chunk, which the
// reader drops unread.
func hugePromptLine(t *testing.T) string {
	return transcriptLine(t, "user", strings.Repeat("x", transcriptChunkBytes+1024))
}

// brokenLine is a line that does not parse.
const brokenLine = `{"type":"user","message":{"role":"user","content":"a prompt the reader cannot parse"`

// pollUntilEnd runs the poller until the run of key ends, a few reads at
// most, and returns its evidence.
func pollUntilEnd(t *testing.T, att *Attachment, key requests.Key) core.Evidence {
	t.Helper()
	var ev core.Evidence
	for range 6 {
		att.pollConfirmations()
		if ev, _ = att.Lookup(key, ""); ev.State.Terminal() {
			break
		}
	}
	return ev
}

const taskText = "list the files of this directory"

// Session grant, Claude Code, end to end: a granted submit writes the stub
// and not the text; the asking prompt the stub raises takes the text from
// the endpoint; the transcript shows the hook's context and the turn's end,
// and the request completes with its answer.
func TestGrantedTaskRunsAtThePromptItsStubRaises(t *testing.T) {
	ft, pid := grantedTarget(t)
	appendTranscript(t, ft.home, closedTurn(t)...)
	stateDir := t.TempDir()
	att := grantedAttach(t, ft, pid, stateDir)

	loc := PromptEndpoints(ft.home, "sess-abc")
	if len(loc) != 1 || loc[0] != (PromptEndpoint{StateDir: stateDir, Target: "cc-1", Instance: att.PromptInstance()}) {
		t.Fatalf("PromptEndpoints = %+v, want this attachment's locator", loc)
	}
	if _, err := Attach(config{Pid: pid, Home: ft.home, Target: "cc-2"}); err != nil {
		t.Fatal(err)
	}
	if got := PromptEndpoints(ft.home, "sess-abc"); len(got) != 1 {
		t.Fatalf("an attachment with no state directory is a prompt endpoint: %+v", got)
	}

	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store, Consent: func(string, string) bool { return true }})
	ep.SetGrant(func(string, string, string) string { return "" })
	ep.Register(att)
	t.Cleanup(func() { _ = ep.Close() })

	// The hook reported the session's mode at an earlier prompt.
	ep.GrantedPrompt(core.PromptReport{TargetID: "cc-1", Instance: loc[0].Instance, Prompt: askingReport})
	const id = "33333333-3333-4333-8333-333333333301"
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: id, TargetID: "cc-1",
		Epoch: SentinelUnpinned, NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
		Input: &protocol.SubmitInput{Text: taskText, Deliver: protocol.DeliverTurn, Busy: protocol.BusyReject}}
	src := core.Source{Host: grantedHost, Origin: map[string]string{"carrier": core.CarrierLink, core.OriginSink: grantedHost},
		Shared: []string{"cc-1"}, NativeSession: "sess-abc", Credential: "cred-1",
		GrantDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000004"}
	out, err := ep.Handle(cmd, src)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(protocol.Reply); r.Outcome.Code != "" || r.Snapshot.State != protocol.StateRunning {
		t.Fatalf("granted submit = %s %q %q, want running", r.Snapshot.State, r.Outcome.Code, r.Outcome.Message)
	}
	env := frameEnvelope(t, ft)
	msgID := frameMsgID(grantedKey(id))
	if strings.Contains(env, taskText) {
		t.Fatal("the frame carries the task text")
	}
	if got, ok := StubMsgID(env); !ok || got != msgID {
		t.Fatalf("StubMsgID(frame) = %q %v, want the frame's msg_id %s", got, ok, msgID)
	}

	text := ep.GrantedPrompt(core.PromptReport{TargetID: "cc-1", Instance: loc[0].Instance, Prompt: askingReport, StubID: msgID, Wait: time.Second})
	if text != taskText {
		t.Fatalf("GrantedPrompt = %q, want the task text", text)
	}
	key := grantedKey(id)
	if rec, _, _ := store.Get(key); !rec.GrantTextGiven {
		t.Fatal("the record does not hold grant_text_given")
	}

	appendTranscript(t, ft.home, deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, text)),
		transcriptLine(t, "assistant", "a.txt b.txt"))
	appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)
	att.pollConfirmations()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec, _, err := store.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State == protocol.StateCompleted && rec.Result != nil && rec.Result.Text == "a.txt b.txt" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("record = %s result %+v, want completed with the answer", rec.State, rec.Result)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session whose working directory came to hold the endpoint's state after
// the stub was sent gets no text at the prompt the stub raises: the request
// ends rejected with the same refusal a submit gives before the send.
func TestGrantedPromptRefusesASessionThatCanWriteTheState(t *testing.T) {
	ft, pid := grantedTarget(t)
	stateDir := t.TempDir()
	att := grantedAttach(t, ft, pid, stateDir)
	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store, Consent: func(string, string) bool { return true }})
	ep.SetGrant(func(string, string, string) string { return "" })
	ep.Register(att)
	t.Cleanup(func() { _ = ep.Close() })

	ep.GrantedPrompt(core.PromptReport{TargetID: "cc-1", Instance: att.PromptInstance(), Prompt: askingReport})
	const id = "33333333-3333-4333-8333-333333333321"
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: id, TargetID: "cc-1",
		Epoch: SentinelUnpinned, NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
		Input: &protocol.SubmitInput{Text: taskText, Deliver: protocol.DeliverTurn, Busy: protocol.BusyReject}}
	src := core.Source{Host: grantedHost, Origin: map[string]string{"carrier": core.CarrierLink, core.OriginSink: grantedHost},
		Shared: []string{"cc-1"}, NativeSession: "sess-abc", Credential: "cred-1",
		GrantDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000006"}
	out, err := ep.Handle(cmd, src)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(protocol.Reply); r.Outcome.Code != "" || r.Snapshot.State != protocol.StateRunning {
		t.Fatalf("granted submit = %s %q %q, want running", r.Snapshot.State, r.Outcome.Code, r.Outcome.Message)
	}

	setRegistry(t, ft.home, pid, map[string]any{"cwd": filepath.Dir(stateDir)})
	if text := ep.GrantedPrompt(core.PromptReport{TargetID: "cc-1", Instance: att.PromptInstance(), Prompt: askingReport,
		StubID: frameMsgID(grantedKey(id)), Wait: time.Second}); text != "" {
		t.Fatalf("GrantedPrompt = %q, want no text", text)
	}
	got, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet,
		RequestRef: protocol.EncodeRef(grantedHost, "cc-1", id)}, src)
	if err != nil {
		t.Fatal(err)
	}
	const want = "grant_unsupported: state_in_workspace: the session can write this machine's state with no dialog"
	if r := got.(protocol.Reply); r.Snapshot.State != protocol.StateRejected || r.Snapshot.Code != protocol.CodeConsentInvalid || r.Outcome.Message != want {
		t.Fatalf("request.get = %s %s %q, want rejected %s %q", r.Snapshot.State, r.Snapshot.Code, r.Outcome.Message, protocol.CodeConsentInvalid, want)
	}
}

// A granted request is refused before anything is sent when the session's
// latest prompt report does not ask, the session can write the endpoint's
// state, it is in a turn, or its state cannot be read.
func TestGrantedSubmitRefusesBeforeTheSend(t *testing.T) {
	rows := []struct {
		name    string
		prepare func(t *testing.T, ft *fakeTarget, pid int, att *Attachment, stateDir string)
		text    string
		code    protocol.Code
		message string
	}{
		{name: "no report", code: protocol.CodeConsentInvalid, message: "grant_needs_signature: mode_unreadable"},
		{name: "a report of another session", prepare: func(_ *testing.T, _ *fakeTarget, _ int, att *Attachment, _ string) {
			att.ObservePrompt(askingReport, time.Now())
			att.ObservePrompt(core.Prompt{NativeSession: "sess-other", Mode: "default"}, time.Now())
		}, code: protocol.CodeConsentInvalid, message: "grant_needs_signature: mode_unreadable"},
		{name: "a mode that does not ask", prepare: func(_ *testing.T, _ *fakeTarget, _ int, att *Attachment, _ string) {
			att.ObservePrompt(core.Prompt{NativeSession: "sess-abc", Mode: "plan"}, time.Now())
		}, code: protocol.CodeConsentInvalid, message: "grant_needs_signature: plan"},
		{name: "the working directory holds the state", prepare: func(t *testing.T, ft *fakeTarget, pid int, att *Attachment, stateDir string) {
			att.ObservePrompt(askingReport, time.Now())
			setRegistry(t, ft.home, pid, map[string]any{"cwd": filepath.Dir(stateDir)})
		}, code: protocol.CodeConsentInvalid, message: "grant_unsupported: state_in_workspace: the session can write this machine's state with no dialog"},
		{name: "busy", prepare: func(t *testing.T, ft *fakeTarget, pid int, att *Attachment, _ string) {
			att.ObservePrompt(askingReport, time.Now())
			setRegistry(t, ft.home, pid, map[string]any{"status": "busy"})
		}, code: protocol.CodeBusy},
		{name: "a state that cannot be read", prepare: func(t *testing.T, ft *fakeTarget, pid int, att *Attachment, _ string) {
			att.ObservePrompt(askingReport, time.Now())
			setRegistry(t, ft.home, pid, map[string]any{"status": ""})
		}, code: protocol.CodeConsentInvalid, message: "grant_needs_signature: the session's state cannot be read"},
		{name: "a text over the limit", prepare: func(_ *testing.T, _ *fakeTarget, _ int, att *Attachment, _ string) {
			att.ObservePrompt(askingReport, time.Now())
		}, text: strings.Repeat("x", GrantedTextLimit+1), code: protocol.CodeConsentInvalid, message: "grant_needs_signature: the text is longer than this session takes under a grant"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ft, pid := grantedTarget(t)
			stateDir := t.TempDir()
			att := grantedAttach(t, ft, pid, stateDir)
			if row.prepare != nil {
				row.prepare(t, ft, pid, att, stateDir)
			}
			text := row.text
			if text == "" {
				text = taskText
			}
			adm, err := att.Submit(grantedRequest("33333333-3333-4333-8333-333333333311", text))
			if err != nil || adm.Admitted || adm.Code != row.code || adm.Message != row.message || !adm.NothingSent {
				t.Fatalf("Submit = %+v, %v; want %s %q with nothing sent", adm, err, row.code, row.message)
			}
			if _, ok := att.runs[grantedKey("33333333-3333-4333-8333-333333333311")]; ok {
				t.Fatal("a refused granted request left a run")
			}
		})
	}
}

// What the attachment reports of a granted run is the digest of the hook
// context the transcript shows, only when that context is whole and the
// stub was the only prompt of its turn.
func TestGrantedRunReportsTheContextItsTurnShows(t *testing.T) {
	cut := func(msgID string) string {
		line, _, _ := strings.Cut(GrantedContext(msgID, taskText), "\n\n")
		return line
	}
	rows := []struct {
		name  string
		lines func(t *testing.T, msgID, env string) []string
		want  string
	}{
		{name: "the whole context", want: sha(taskText), lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "a context cut after its first line", lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, cut(msgID)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "a preview that holds the first line", lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, cut(msgID)+"\n\nPreview (first 2KB):\n"+taskText[:8]), transcriptLine(t, "assistant", "done")}
		}},
		{name: "another message id", lines: func(t *testing.T, msgID, env string) []string {
			other := frameMsgID(grantedKey("33333333-3333-4333-8333-333333333399"))
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(other, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "a prompt before the stub in its turn", lines: func(t *testing.T, msgID, env string) []string {
			return []string{transcriptLine(t, "user", "the person's queued prompt"), deliveredLine(t, msgID, env),
				hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "a prompt folded into the stub's running turn", lines: func(t *testing.T, msgID, env string) []string {
			other := frameMsgID(grantedKey("33333333-3333-4333-8333-333333333398"))
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)),
				transcriptLine(t, "assistant", "working"), absorbedLine(t, other, "another message"), transcriptLine(t, "assistant", "done")}
		}},
		{name: "a stub folded into a running turn", lines: func(t *testing.T, msgID, env string) []string {
			return []string{transcriptLine(t, "user", "the person's prompt"), transcriptLine(t, "assistant", "working"),
				absorbedLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		// Review finding: a prompt of an image alone is a prompt, and a line
		// the reader drops or cannot parse is a gap that may hide one.
		{name: "an image-only prompt before the stub in its turn", lines: func(t *testing.T, msgID, env string) []string {
			return []string{imageLine(t), deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "an image-only prompt after the stub in its turn", lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)), imageLine(t), transcriptLine(t, "assistant", "done")}
		}},
		{name: "an over-size line in the stub's turn", lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), hugePromptLine(t), hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "an unparseable line in the stub's turn", lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), brokenLine, hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		{name: "a gap in an earlier turn", want: sha(taskText), lines: func(t *testing.T, msgID, env string) []string {
			return []string{brokenLine, transcriptLine(t, "assistant", "an answer after the gap"),
				deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done")}
		}},
		// A gap while the run still runs may hide a prompt folded into it.
		{name: "a gap after the run's first answer line", lines: func(t *testing.T, msgID, env string) []string {
			return []string{deliveredLine(t, msgID, env), hookContextLine(t, GrantedContext(msgID, taskText)),
				transcriptLine(t, "assistant", "working"), brokenLine, transcriptLine(t, "assistant", "done")}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ft, pid := grantedTarget(t)
			appendTranscript(t, ft.home, closedTurn(t)...)
			att := grantedAttach(t, ft, pid, t.TempDir())
			att.ObservePrompt(askingReport, time.Now())
			const id = "33333333-3333-4333-8333-333333333321"
			adm, err := att.Submit(grantedRequest(id, taskText))
			if err != nil || !adm.Admitted {
				t.Fatalf("Submit = %+v, %v", adm, err)
			}
			appendTranscript(t, ft.home, row.lines(t, adm.RunID, frameEnvelope(t, ft))...)
			appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)
			ev := pollUntilEnd(t, att, grantedKey(id))
			if ev.State != protocol.StateCompleted || ev.GrantedTextDigest != row.want {
				t.Fatalf("evidence = %s digest %q, want completed with digest %q", ev.State, ev.GrantedTextDigest, row.want)
			}
		})
	}

	// A signed message delivered after the stub and before the first
	// assistant line shares its turn: the granted run reports nothing, and
	// the signed run completes as it does today.
	t.Run("a signed run after the stub", func(t *testing.T) {
		ft, pid := grantedTarget(t)
		appendTranscript(t, ft.home, closedTurn(t)...)
		att := grantedAttach(t, ft, pid, t.TempDir())
		att.ObservePrompt(askingReport, time.Now())
		const grantedID, signedID = "33333333-3333-4333-8333-333333333331", "33333333-3333-4333-8333-333333333332"
		adm, err := att.Submit(grantedRequest(grantedID, taskText))
		if err != nil || !adm.Admitted {
			t.Fatalf("granted Submit = %+v, %v", adm, err)
		}
		stubEnv := frameEnvelope(t, ft)
		signed := grantedRequest(signedID, "a signed task")
		signed.Granted = false
		sadm, err := att.Submit(signed)
		if err != nil || !sadm.Admitted {
			t.Fatalf("signed Submit = %+v, %v", sadm, err)
		}
		appendTranscript(t, ft.home, deliveredLine(t, adm.RunID, stubEnv), hookContextLine(t, GrantedContext(adm.RunID, taskText)),
			deliveredLine(t, sadm.RunID, frameEnvelope(t, ft)), transcriptLine(t, "assistant", "signed answer"))
		appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)
		att.pollConfirmations()
		if ev, _ := att.Lookup(grantedKey(grantedID), ""); ev.GrantedTextDigest != "" || ev.State.Terminal() {
			t.Fatalf("granted run = %s digest %q, want no end and no digest", ev.State, ev.GrantedTextDigest)
		}
		if ev, _ := att.Lookup(grantedKey(signedID), ""); ev.State != protocol.StateCompleted || ev.Result == nil || ev.Result.Text != "signed answer" || ev.GrantedTextDigest != "" {
			t.Fatalf("signed run = %s %+v digest %q, want completed with its answer", ev.State, ev.Result, ev.GrantedTextDigest)
		}
	})
}

// A run that a restarted attachment rebuilds from the transcript is judged
// by its whole turn: the reader reads back to the turn's start.
func TestRecoveredGrantedRunReadsBackToItsTurn(t *testing.T) {
	stub := func(t *testing.T, id string) (string, string) {
		msgID := frameMsgID(grantedKey(id))
		return msgID, deliveredLine(t, msgID, stubEnvelope(t, msgID))
	}
	rows := []struct {
		name   string
		before func(t *testing.T) []string
		// after are lines between the stub and its context.
		after []string
		want  bool
	}{
		{name: "the only prompt of its turn", want: true, before: func(t *testing.T) []string { return closedTurn(t) }},
		{name: "the second of two stubs of one turn", before: func(t *testing.T) []string {
			_, first := stub(t, "33333333-3333-4333-8333-333333333340")
			return append(closedTurn(t), first)
		}},
		{name: "history that starts inside the turn", before: func(t *testing.T) []string {
			huge, err := json.Marshal(map[string]any{"type": "system", "content": strings.Repeat("x", transcriptChunkBytes+1024)})
			if err != nil {
				t.Fatal(err)
			}
			return append(closedTurn(t), string(huge))
		}},
		// Review finding: the read back and the replay judge the same gaps
		// as the live reader.
		{name: "an image-only prompt before the stub in its turn", before: func(t *testing.T) []string {
			return append(closedTurn(t), imageLine(t))
		}},
		{name: "an unparseable line before the stub in its turn", before: func(t *testing.T) []string {
			return append(closedTurn(t), brokenLine)
		}},
		{name: "an over-size line after the stub", before: closedTurn, after: []string{hugePromptLine(t)}},
		{name: "a gap in an earlier turn", want: true, before: func(t *testing.T) []string {
			return append(closedTurn(t), brokenLine, transcriptLine(t, "assistant", "an answer after the gap"))
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ft, pid := grantedTarget(t)
			const id = "33333333-3333-4333-8333-333333333341"
			msgID, line := stub(t, id)
			appendTranscript(t, ft.home, row.before(t)...)
			appendTranscript(t, ft.home, line)
			if len(row.after) > 0 {
				appendTranscript(t, ft.home, row.after...)
			}
			appendTranscript(t, ft.home, hookContextLine(t, GrantedContext(msgID, taskText)), transcriptLine(t, "assistant", "done"))
			appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)

			att := grantedAttach(t, ft, pid, t.TempDir())
			if ev, _ := att.Lookup(grantedKey(id), ""); ev.Class != core.EvidenceTentative {
				t.Fatalf("recovery = %s, want the run rebuilt", ev.Class)
			}
			ev := pollUntilEnd(t, att, grantedKey(id))
			want := ""
			if row.want {
				want = sha(taskText)
			}
			if ev.State != protocol.StateCompleted || ev.GrantedTextDigest != want {
				t.Fatalf("recovered run = %s digest %q, want completed with digest %q", ev.State, ev.GrantedTextDigest, want)
			}
		})
	}
}

// Review finding: the prompt hook's discovery is bounded, so its deadline
// covers it. It reads at most 16 sentinels in name order, also when they are
// empty: a locator that sorts 16th is found, one that sorts 17th is not.
func TestPromptEndpointsReadsAtMostSixteenSentinels(t *testing.T) {
	for _, row := range []struct {
		empties int
		found   bool
	}{{empties: 15, found: true}, {empties: 16, found: false}} {
		home := t.TempDir()
		dir := stopBoundDir(home, "sess-abc")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := range row.empties {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("a%02d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stateDir := t.TempDir()
		loc, err := json.Marshal(promptLocator{StateDir: stateDir, Target: "cc-1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b-valid"), loc, 0o600); err != nil {
			t.Fatal(err)
		}
		got := PromptEndpoints(home, "sess-abc")
		if found := len(got) == 1 && got[0] == (PromptEndpoint{StateDir: stateDir, Target: "cc-1", Instance: "b-valid"}); found != row.found || len(got) > 1 {
			t.Fatalf("%d empty sentinels before the locator: PromptEndpoints = %+v, want found %v", row.empties, got, row.found)
		}
	}
}

// A report dropped after a request that ended untaken is only one observed
// before that end: a newer report stays.
func TestForgetPromptKeepsANewerReport(t *testing.T) {
	ft, pid := grantedTarget(t)
	att := grantedAttach(t, ft, pid, t.TempDir())
	at := time.Now()
	att.ObservePrompt(askingReport, at)
	att.ForgetPrompt(at.Add(-time.Second))
	if asks, why := att.SessionAsks(); !asks {
		t.Fatalf("after a forget older than the report: asks false (%s)", why)
	}
	att.ForgetPrompt(at.Add(time.Second))
	if asks, _ := att.SessionAsks(); asks {
		t.Fatal("a forget newer than the report kept it")
	}
}

func TestStubMsgID(t *testing.T) {
	id := frameMsgID(grantedKey("33333333-3333-4333-8333-333333333351"))
	wrap := func(body string) string {
		return `<cross-session-message from="amq-cc-1" from-session="amq" from-name="amq-remote" from-mode="code">` + "\n" + body + "\n</cross-session-message>"
	}
	rows := []struct {
		name, prompt string
		ok           bool
	}{
		{"the wrapper with a stub", wrap(grantedStub(id)), true},
		{"a stub among other text", wrap("please also\n" + grantedStub(id)), false},
		{"a bare stub", grantedStub(id), false},
		{"another tag around a stub", "<note>\n" + grantedStub(id) + "\n</cross-session-message>", false},
		{"a bad id", wrap(grantedStub("p3-0001")), false},
	}
	for _, row := range rows {
		if got, ok := StubMsgID(row.prompt); ok != row.ok || (ok && got != id) {
			t.Errorf("%s: StubMsgID = %q %v, want ok %v", row.name, got, ok, row.ok)
		}
	}
}

// The prompt hook reads its session, mode and stub from its input, and
// prints the context in the shape Claude Code takes as added context.
func TestPromptHookInputAndOutput(t *testing.T) {
	id := frameMsgID(grantedKey("33333333-3333-4333-8333-333333333361"))
	input, _ := json.Marshal(map[string]any{"session_id": "sess-abc", "permission_mode": "acceptEdits", "hook_event_name": "UserPromptSubmit",
		"prompt": "<cross-session-message from=\"amq-cc-1\">\n" + grantedStub(id) + "\n</cross-session-message>"})
	in, ok := ParsePromptHookInput(bytes.NewReader(input))
	if !ok || in != (PromptHookInput{SessionID: "sess-abc", Mode: "acceptEdits", StubID: id}) {
		t.Fatalf("ParsePromptHookInput = %+v %v", in, ok)
	}
	stop, _ := json.Marshal(map[string]any{"session_id": "sess-abc", "hook_event_name": "Stop"})
	if _, ok := ParsePromptHookInput(bytes.NewReader(stop)); ok {
		t.Fatal("a Stop input parsed as a prompt")
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(PromptHookOutput(id, taskText), &out); err != nil {
		t.Fatal(err)
	}
	if out.HookSpecificOutput.HookEventName != "UserPromptSubmit" || out.HookSpecificOutput.AdditionalContext != GrantedContext(id, taskText) {
		t.Fatalf("PromptHookOutput = %+v", out)
	}
	if text, ok := grantedContextText(out.HookSpecificOutput.AdditionalContext, id); !ok || text != taskText {
		t.Fatalf("the printed context does not read back whole: %q %v", text, ok)
	}
}
