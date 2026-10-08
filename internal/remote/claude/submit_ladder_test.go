package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// transcriptLine renders one VALID transcript JSONL entry: the content
// goes through json.Marshal so newlines and quotes are escaped exactly as
// Claude Code writes them (codex #855 r1 item 2: the previous fixture
// spliced a raw envelope into a JSON string, which was not JSON at all).
func transcriptLine(t *testing.T, typ, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":      typ,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message":   map[string]any{"role": typ, "content": content},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// deliveredLine is the idle-target delivery shape from the 611.2 probe
// transcript: a user entry wrapping the envelope, with origin.msg_id set
// to the frame's msg_id.
func deliveredLine(t *testing.T, msgID, envelope string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":      "user",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message":   map[string]any{"role": "user", "content": harnessUserText(envelope)},
		"origin":    map[string]any{"kind": "peer", "from": "unknown", "msg_id": msgID, "name": "amq-remote"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// absorbedLine is the busy-target delivery shape observed live on
// 2026-09-22 (v2.1.278): the frame absorbed mid-turn as an attachment of
// type queued_command carrying attachment.origin.msg_id.
func absorbedLine(t *testing.T, msgID, envelope string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":      "attachment",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"attachment": map[string]any{
			"type":        "queued_command",
			"prompt":      envelope,
			"commandMode": "prompt",
			"origin":      map[string]any{"kind": "peer", "from": "amq-cc-1", "msg_id": msgID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// appendTranscript appends complete JSONL lines, as the harness does.
func appendTranscript(t *testing.T, home string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", slugifyCwd("/tmp/proj"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "sess-abc.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func appendStopMarker(t *testing.T, home string, tsMillis int64) {
	t.Helper()
	p := stopMarkerPath(home, "sess-abc")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	line, _ := json.Marshal(map[string]any{"ts": tsMillis, "session_id": "sess-abc"})
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// harnessUserText is the capture §3.2 wrapper: preamble + exact envelope
// + guidance prose, all inside one JSON string.
func harnessUserText(envelope string) string {
	return "Another Claude session sent a message: " + envelope + "  This came from another Claude session — not typed by your user."
}

// TestConfirmationLadderOnValidJSONL drives the ladder with real transcript
// JSONL: user line (submitted) → assistant line (admitted) → Stop marker
// (completed, result = the assistant text) → ack releases the record.
// Regression for codex #855 r1 items 2 and 7.
func TestConfirmationLadderOnValidJSONL(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	var got []core.NativeEvent
	att.Subscribe(func(ev core.NativeEvent) { got = append(got, ev) })

	admission, err := att.Submit(pr2BoundRequest("ladder-probe-body"))
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	key := pr2Key()
	env := frameEnvelope(t, ft)
	userLine := deliveredLine(t, admission.RunID, env)
	if strings.Contains(userLine, env) {
		t.Fatal("fixture leaks the raw envelope into the JSONL; escaping did not happen")
	}

	appendTranscript(t, ft.home, userLine)
	att.pollConfirmations()
	ev, _ := att.Lookup(key, "")
	if ev.Class != core.EvidenceTentative || !att.runs[key].submitted {
		t.Fatalf("after user line: class=%s submitted=%v, want tentative+submitted", ev.Class, att.runs[key].submitted)
	}

	appendTranscript(t, ft.home, transcriptLine(t, "assistant", "working on it"))
	att.pollConfirmations()
	ev, _ = att.Lookup(key, "")
	if ev.Class != core.EvidenceConfirmed || !ev.Admitted || ev.RunID != admission.RunID {
		t.Fatalf("after assistant line: %+v, want admitted with RunID %s", ev, admission.RunID)
	}

	appendTranscript(t, ft.home, transcriptLine(t, "assistant", "pong"))
	appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)
	att.pollConfirmations()
	ev, _ = att.Lookup(key, "")
	if ev.State != protocol.StateCompleted || ev.Result == nil || ev.Result.Text != "pong" {
		t.Fatalf("after stop: state=%s result=%+v, want completed with text pong", ev.State, ev.Result)
	}
	var completed int
	for _, e := range got {
		if e.Type == core.EventRunCompleted && e.Key == key {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("EventRunCompleted emitted %d times, want 1", completed)
	}

	att.AcknowledgeResult(key, "", "")
	if ev, _ := att.Lookup(key, ""); ev.Class != core.EvidenceUnknown {
		t.Fatalf("after ack: %s, want unknown", ev.Class)
	}
}

// TestStopMarkerBindsToRunNotToSize pins codex #855 r1 item 7: an old
// marker (before the run existed) never completes the run, and a marker
// observed while the transcript has not yet shown admission is preserved
// and completes the run once admission is observed.
func TestStopMarkerBindsToRunNotToSize(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)

	// Stale marker from a turn that ended before we ever submitted.
	appendStopMarker(t, ft.home, time.Now().Add(-time.Minute).UnixMilli())

	admission, err := att.Submit(pr2BoundRequest("bind-probe"))
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	key := pr2Key()
	userLine := deliveredLine(t, admission.RunID, frameEnvelope(t, ft))

	// Tick 1: transcript shows only the user line; a FRESH stop marker is
	// already on disk (hook fired before the transcript flushed the
	// assistant entry). Old marker → dropped; fresh marker → preserved.
	appendTranscript(t, ft.home, userLine)
	appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)
	att.pollConfirmations()
	if rec := att.runs[key]; rec.terminal || rec.admitted {
		t.Fatalf("tick 1: terminal=%v admitted=%v; a stop must not complete an unadmitted run", rec.terminal, rec.admitted)
	}

	// Tick 2: the assistant entry lands. The preserved marker now completes
	// exactly this run.
	appendTranscript(t, ft.home, transcriptLine(t, "assistant", "done"))
	att.pollConfirmations()
	rec := att.runs[key]
	if !rec.admitted || !rec.terminal || rec.result == nil || rec.result.Text != "done" {
		t.Fatalf("tick 2: admitted=%v terminal=%v result=%+v, want the preserved stop to complete the run", rec.admitted, rec.terminal, rec.result)
	}
}

// TestCompletionCallbackMayReacquireMutex pins codex #855 r1 item 6: the
// endpoint acknowledges a completed run synchronously from the event
// callback (core/endpoint.go AcknowledgeResult path), which takes a.mu.
// Emitting under a.mu deadlocked exactly when confirmation worked.
func TestCompletionCallbackMayReacquireMutex(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	key := pr2Key()
	att.Subscribe(func(ev core.NativeEvent) {
		if ev.Type == core.EventRunCompleted {
			att.AcknowledgeResult(ev.Key, "", "")
		}
	})
	admission, err := att.Submit(pr2BoundRequest("deadlock-probe"))
	if err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, ft.home, deliveredLine(t, admission.RunID, frameEnvelope(t, ft)), transcriptLine(t, "assistant", "ok"))
	appendStopMarker(t, ft.home, time.Now().UnixMilli()+1)

	done := make(chan struct{})
	go func() {
		att.pollConfirmations()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollConfirmations deadlocked: completion callback reacquired a.mu while it was held")
	}
	if ev, _ := att.Lookup(key, ""); ev.Class != core.EvidenceUnknown {
		t.Fatalf("record not released by the synchronous ack: %+v", ev)
	}
}

// frameEnvelope returns the envelope the fake target received on the wire
// for the latest submit (the frame line after the auth line).
func frameEnvelope(t *testing.T, ft *fakeTarget) string {
	t.Helper()
	raw := waitRecv(t, ft)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var f Frame
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &f); err != nil {
		t.Fatalf("frame line: %v", err)
	}
	return f.Message.Content
}

// Live smoke 2026-09-22 against a busy target: the frame was absorbed
// mid-turn as a queued_command attachment, never as a user entry, and the
// poller stayed at tentative. The attachment's origin.msg_id must grade
// the run submitted, then admitted on the following assistant entry.
func TestAbsorbedMidTurnFrameClimbsTheLadder(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	admission, err := att.Submit(pr2BoundRequest("absorbed-probe"))
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	key := pr2Key()
	appendTranscript(t, ft.home,
		transcriptLine(t, "assistant", "busy with something else"),
		absorbedLine(t, admission.RunID, frameEnvelope(t, ft)),
		transcriptLine(t, "assistant", "handling the peer request"))
	att.pollConfirmations()
	ev, _ := att.Lookup(key, "")
	if !att.runs[key].submitted || ev.Class != core.EvidenceConfirmed || !ev.Admitted {
		t.Fatalf("absorbed frame: submitted=%v evidence=%+v, want submitted and admitted", att.runs[key].submitted, ev)
	}
}

// codex #855 r2 item 1 (reproduced by the reviewer): a large entry between
// the delivery and the first assistant entry pushed the delivery out of the
// fixed 256 KiB tail, and the run stayed tentative forever. The cursor
// follows the file from the submit-time offset, so the delivery is seen
// once and remembered.
func TestAdmissionSurvivesLargeEntryAfterDelivery(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	admission, err := att.Submit(pr2BoundRequest("rollover-probe"))
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	key := pr2Key()
	appendTranscript(t, ft.home, deliveredLine(t, admission.RunID, frameEnvelope(t, ft)))
	att.pollConfirmations()
	if !att.runs[key].submitted {
		t.Fatal("setup: delivery was not observed")
	}
	large, _ := json.Marshal(map[string]any{"type": "progress", "data": strings.Repeat("x", 300<<10)})
	appendTranscript(t, ft.home, string(large), transcriptLine(t, "assistant", "the result"))
	att.pollConfirmations()
	if !att.runs[key].admitted || att.runs[key].lastText != "the result" {
		t.Fatalf("admitted=%v lastText=%q: the delivery was forgotten once a large entry followed it", att.runs[key].admitted, att.runs[key].lastText)
	}
}

// codex #855 r2 item 2 (reproduced by the reviewer): our delivery, then an
// unrelated local prompt, then its answer. The answer must not admit our
// run, and a Stop after that prompt must not complete it.
func TestUnrelatedLaterPromptNeverAdmitsOrCompletes(t *testing.T) {
	ft := newFakeTarget(t, 4242, nil)
	att := ft.attach(t)
	admission, err := att.Submit(pr2BoundRequest("turn-probe"))
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	key := pr2Key()
	appendTranscript(t, ft.home,
		deliveredLine(t, admission.RunID, frameEnvelope(t, ft)),
		transcriptLine(t, "user", "a different local request"),
		transcriptLine(t, "assistant", "local request answer"))
	appendStopMarker(t, ft.home, time.Now().UnixMilli()+50)
	att.pollConfirmations()
	rec := att.runs[key]
	if rec.admitted || rec.terminal {
		t.Fatalf("admitted=%v terminal=%v: another turn's answer or Stop was attributed to our run", rec.admitted, rec.terminal)
	}
}

// agent-message-queue-611.66 (observed live): the user interrupted a
// Buzz-driven turn in the terminal. Claude Code wrote the interrupt line and
// ran no Stop hook; the line read as a new prompt, so the run never settled
// and the endpoint refused every later submit as busy. Review of #1003: when
// the user submits a new prompt mid-turn, the interrupt line carries the new
// prompt's id.
func TestInterruptedTurnCancelsItsRun(t *testing.T) {
	for _, interruptPrompt := range []string{"p-1", "p-2"} {
		t.Run(interruptPrompt, func(t *testing.T) {
			ft := newFakeTarget(t, 4242, nil)
			att := ft.attach(t)
			got := make(chan core.NativeEvent, 32)
			att.Subscribe(func(ev core.NativeEvent) { got <- ev })
			admission, err := att.Submit(pr2BoundRequest("interrupt-probe"))
			if err != nil || !admission.Admitted {
				t.Fatalf("submit: %+v %v", admission, err)
			}
			withPrompt := func(id, line string) string {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatal(err)
				}
				m["promptId"] = id
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			appendTranscript(t, ft.home,
				withPrompt("p-1", deliveredLine(t, admission.RunID, frameEnvelope(t, ft))),
				transcriptLine(t, "assistant", "partial answer"),
				withPrompt(interruptPrompt, transcriptLine(t, "user", "[Request interrupted by user for tool use]")))
			att.pollConfirmations()

			att.mu.Lock()
			rec := att.runs[pr2Key()]
			terminal, state := rec.terminal, rec.state
			att.mu.Unlock()
			if !terminal || state != protocol.StateCancelled {
				t.Fatalf("terminal=%v state=%s, want a cancelled run", terminal, state)
			}
			var local, cancelled bool
			for len(got) > 0 {
				switch ev := <-got; ev.Type {
				case core.EventLocalIntervention:
					local = true
				case core.EventRunCancelled:
					cancelled = true
				case core.EventRunCompleted:
					t.Fatal("an interrupted turn reported completed")
				}
			}
			if !local || !cancelled {
				t.Fatalf("local_intervention=%v run_cancelled=%v, want both", local, cancelled)
			}
		})
	}
}
