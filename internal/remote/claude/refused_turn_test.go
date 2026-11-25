package claude

// Refused-turn sequences from live Claude Code 2.1.331 sessions. Identifiers,
// paths, names and unrelated content in the fixtures are synthetic. Roles,
// structural flags and turn order preserve the recorded entries.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// refusedTurnReason is the failure reason the adapter gives; the test
// constant asserts the exact string.
const refusedTurnReason = "the turn ended in an API error and the session moved on"

// refusedEvents captures the native events the attachment emits, and checks
// that no event is emitted while the attachment mutex is held.
type refusedEvents struct {
	mu  sync.Mutex
	evs []core.NativeEvent
}

func (r *refusedEvents) subscribe(att *Attachment) {
	att.Subscribe(func(ev core.NativeEvent) {
		if att.mu.TryLock() {
			att.mu.Unlock()
		} else {
			panic("native event emitted under the attachment mutex")
		}
		r.mu.Lock()
		r.evs = append(r.evs, ev)
		r.mu.Unlock()
	})
}

func (r *refusedEvents) terminals(runID string) []core.NativeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.NativeEvent
	for _, ev := range r.evs {
		if ev.RunID == runID && (ev.Type == core.EventRunCompleted || ev.Type == core.EventRunFailed || ev.Type == core.EventRunCancelled) {
			out = append(out, ev)
		}
	}
	return out
}

// refusedState is the run record's assertion surface.
type refusedState struct {
	submitted, admitted, terminal, granted bool
	shared, turnKnown                      bool
	state                                  protocol.State
	result                                 *protocol.Result
}

func refusedRecord(t *testing.T, att *Attachment, key requests.Key) refusedState {
	t.Helper()
	att.mu.Lock()
	defer att.mu.Unlock()
	rec, ok := att.runs[key]
	if !ok {
		t.Fatal("no run record")
	}
	return refusedState{submitted: rec.submitted, admitted: rec.admitted, terminal: rec.terminal,
		state: rec.state, result: rec.result, granted: rec.granted, shared: rec.shared, turnKnown: rec.turnKnown}
}

// wantRefusedFailure asserts the one terminal outcome of a refused run left
// behind: failed, no result, exactly one terminal event, its reason set.
func wantRefusedFailure(t *testing.T, got refusedState, evs []core.NativeEvent) {
	t.Helper()
	if !got.terminal || got.state != protocol.StateFailed || got.result != nil {
		t.Fatalf("want failed with no result: terminal=%v state=%s result=%+v", got.terminal, got.state, got.result)
	}
	if len(evs) != 1 || evs[0].Type != core.EventRunFailed || evs[0].Result != nil || evs[0].Reason != refusedTurnReason {
		t.Fatalf("want exactly one result-free failed event with the refusal reason: %+v", evs)
	}
}

// wantRefusedCompletion asserts the healthy continuation outcome.
func wantRefusedCompletion(t *testing.T, got refusedState, text string) {
	t.Helper()
	if !got.terminal || got.state != protocol.StateCompleted || got.result == nil || got.result.Text != text {
		t.Fatalf("want completed with %q: terminal=%v state=%s result=%+v", text, got.terminal, got.state, got.result)
	}
}

func refusedLine(t *testing.T, fields map[string]any) string {
	t.Helper()
	if fields["timestamp"] == nil {
		fields["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func refusedAssistant(t *testing.T, text string, apiError bool) string {
	t.Helper()
	fields := map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": text}}
	if apiError {
		fields["isApiErrorMessage"] = true
		fields["message"].(map[string]any)["model"] = "<synthetic>"
	}
	return refusedLine(t, fields)
}

func refusedUser(t *testing.T, text string) string {
	t.Helper()
	return refusedLine(t, map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "message": map[string]any{"role": "user", "content": text}})
}

// refusedFixtureLines reads one sanitized fixture, the entries the saved
// transcript held for the live run it covers.
func refusedFixtureLines(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "refused-turn", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// rebindRefusedDelivery rewrites the fixture's peer delivery so its
// origin.msg_id names THIS test's submitted run. Only origin.msg_id and
// origin.from change; the text and every other field stay as recorded.
func rebindRefusedDelivery(t *testing.T, line, runID string) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatal(err)
	}
	origin, ok := fields["origin"].(map[string]any)
	if !ok {
		t.Fatalf("fixture line is not a peer delivery: %.80s", line)
	}
	origin["msg_id"] = runID
	origin["from"] = "amq-cc-test"
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// replayRefusedTranscript feeds fixture lines in file order through the
// attachment's own poller, one entry per poll, with the delivery line
// rebound to the run. A Stop marker is written only for a sequence whose
// live turn ran Stop. The live hook ran before the transcript flushed the
// turn's lines, so a preserved marker binds once the cursor catches up
// (TestStopMarkerBindsToRunNotToSize pins that order); the marker is dated
// after the transcript's last entry.
func replayRefusedTranscript(t *testing.T, att *Attachment, home string, lines []string, runID string, stop bool) {
	t.Helper()
	var last time.Time
	rebound := false
	for _, line := range lines {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		origin, _ := fields["origin"].(map[string]any)
		if origin != nil && origin["msg_id"] != "" {
			line = rebindRefusedDelivery(t, line, runID)
			rebound = true
		}
		if ts, _ := fields["timestamp"].(string); ts != "" {
			if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				last = at
			}
		}
		appendTranscript(t, home, line)
		att.pollConfirmations()
	}
	if !rebound {
		t.Fatal("fixture has no peer delivery for its submitted key to match")
	}
	if stop {
		appendStopMarker(t, home, last.Add(time.Minute+5*time.Millisecond).UnixMilli())
	}
	att.pollConfirmations()
}

// submitTurnRun submits one link task on the plain target and waits for the
// transcript delivery line that proves the harness accepted it.
func submitTurnRun(t *testing.T, att *Attachment, ft *fakeTarget) core.Admission {
	t.Helper()
	admission, err := att.Submit(pr2BoundRequest("refused turn probe"))
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	appendTranscript(t, ft.home, deliveredLine(t, admission.RunID, frameEnvelope(t, ft)))
	att.pollConfirmations()
	return admission
}

// A granted run's turn is refused; the person runs /rate-limit-options whose
// rows are `rowSource:"local"`; the harness's continuation starts a new turn;
// the model answers with a Stop. The task must complete with that answer,
// grant attribution untouched. On main the local rows end the run's turn, so
// the later Stop is unclaimable and the run never completes: RED on main.
func TestRefusedTurnLocalCommandsAllowGrantedContinuation(t *testing.T) {
	ft, pid := grantedTarget(t)
	att := grantedAttach(t, ft, pid, t.TempDir())
	att.ObservePrompt(askingReport, time.Now())

	req := grantedRequest("55555555-5555-4555-8555-555555555544", "granted continuation replay")
	admission, err := att.Submit(req)
	if err != nil || !admission.Admitted {
		t.Fatalf("submit: %+v %v", admission, err)
	}
	key := grantedKey("55555555-5555-4555-8555-555555555544")
	replayRefusedTranscript(t, att, ft.home, refusedFixtureLines(t, "granted-continuation.jsonl"), admission.RunID, true)
	got := refusedRecord(t, att, key)
	if !got.submitted || !got.turnKnown || got.shared {
		t.Fatalf("grant evidence lost its original turn: %+v", got)
	}
	wantRefusedCompletion(t, got, "GRANT-CLAUDE-02")
}

// Core owns the attachment's single subscription here: the refused task ends
// failed/native_error with no result and holds no reservation, so the second
// submit is admitted through the real Endpoint and completes with its own
// answer. On main the refused run stays running and the second submit reads
// busy: RED on main.
func TestRefusedTurnTypedPromptClosesAndAllowsAnotherRequest(t *testing.T) {
	ep, store, att, ft, _ := releaseFixture(t, os.Getpid())
	first := releaseSubmit(t, ep, pr2Key().RequestID, time.Minute)
	if first.Snapshot.State.Terminal() {
		t.Fatalf("first submit closed at once: %+v", first.Snapshot)
	}
	replayRefusedTranscript(t, att, ft.home, refusedFixtureLines(t, "refusal-typed-prompt.jsonl"), *first.Snapshot.NativeRun, false)
	rec, ok, err := store.Get(pr2Key())
	if err != nil || !ok {
		t.Fatalf("read first terminal record: found=%v err=%v", ok, err)
	}
	if rec.State != protocol.StateFailed || rec.Code != protocol.CodeNativeError || rec.Result != nil || rec.RefusalReason != refusedTurnReason {
		t.Fatalf("first task = %s/%s result=%+v reason=%q", rec.State, rec.Code, rec.Result, rec.RefusalReason)
	}
	second := releaseSubmit(t, ep, "55555555-5555-4555-8555-555555555602", time.Minute)
	if second.Snapshot.Code == protocol.CodeBusy || second.Snapshot.NativeRun == nil || second.Snapshot.State.Terminal() {
		t.Fatalf("second submit = %+v: the failed run still reserves the session", second.Snapshot)
	}
	id := *second.Snapshot.NativeRun
	appendTranscript(t, ft.home, deliveredLine(t, id, frameEnvelope(t, ft)), refusedAssistant(t, "second answer", false))
	att.pollConfirmations()
	appendStopMarker(t, ft.home, time.Now().Add(time.Millisecond).UnixMilli())
	att.pollConfirmations()
	secondRec, ok, err := store.Get(requests.Key{CreatorHost: core.LocalHost, TargetID: "cc-1", RequestID: second.Snapshot.RequestID})
	if err != nil || !ok || secondRec.State != protocol.StateCompleted || secondRec.Result == nil || secondRec.Result.Text != "second answer" {
		t.Fatalf("second task = %s result=%+v found=%v err=%v", secondRec.State, secondRec.Result, ok, err)
	}
}

// A second delivery ends only the genuinely left-behind refused owner; the
// new run owns the turn and completes.
func TestRefusedTurnOtherDeliveryFailsOnlyRefusedRun(t *testing.T) {
	ft := newFakeTarget(t, os.Getpid(), nil)
	att := ft.attach(t)
	events := &refusedEvents{}
	events.subscribe(att)
	first := submitTurnRun(t, att, ft)
	appendTranscript(t, ft.home, refusedAssistant(t, "API Error: you've hit your limit", true))
	att.pollConfirmations()
	reqB := grantedRequest("33333333-3333-4333-8333-3333333333a1", "other delivery")
	reqB.Granted = false
	second, err := att.Submit(reqB)
	if err != nil || !second.Admitted {
		t.Fatalf("second submit: %+v %v", second, err)
	}
	appendTranscript(t, ft.home, deliveredLine(t, second.RunID, frameEnvelope(t, ft)), refusedAssistant(t, "second answer", false))
	att.pollConfirmations()
	appendStopMarker(t, ft.home, time.Now().Add(time.Millisecond).UnixMilli())
	att.pollConfirmations()
	wantRefusedFailure(t, refusedRecord(t, att, pr2Key()), events.terminals(first.RunID))
	wantRefusedCompletion(t, refusedRecord(t, att, grantedKey("33333333-3333-4333-8333-3333333333a1")), "second answer")
}

// Command-like wrapper text inside a person's own prompt is not structural
// harness evidence: rowSource is the only command-row discriminator.
// Review finding, 2026-11-25: was a text test for command rows, red on main.
func TestRefusedTurnCommandLookingTextEndsAsFailed(t *testing.T) {
	ft := newFakeTarget(t, os.Getpid(), nil)
	att := ft.attach(t)
	events := &refusedEvents{}
	events.subscribe(att)
	admission := submitTurnRun(t, att, ft)
	appendTranscript(t, ft.home,
		refusedAssistant(t, "API Error: you've hit your limit", true),
		refusedUser(t, "<command-name>/rate-limit-options</command-name> and explain rate limits instead"),
		refusedAssistant(t, "the person's own answer", false))
	att.pollConfirmations()
	wantRefusedFailure(t, refusedRecord(t, att, pr2Key()), events.terminals(admission.RunID))
}

// A Stop whose time is before a later prompt still binds to the earlier run,
// also when both arrive in one poll.
// Review finding, 2026-11-25: the old boundary failed any run, not only a refused one.
func TestAnswerStopAndLaterPromptCompletesOriginalRun(t *testing.T) {
	ft := newFakeTarget(t, os.Getpid(), nil)
	att := ft.attach(t)
	submitTurnRun(t, att, ft)
	base := time.Now()
	answer := refusedLine(t, map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "the answer"}, "timestamp": base.Add(10 * time.Millisecond).UTC().Format(time.RFC3339Nano)})
	prompt := refusedLine(t, map[string]any{"type": "user", "origin": map[string]any{"kind": "human"}, "message": map[string]any{"role": "user", "content": "another task"}, "timestamp": base.Add(30 * time.Millisecond).UTC().Format(time.RFC3339Nano)})
	appendTranscript(t, ft.home, answer, prompt)
	// The marker is dated between the answer and the later prompt: inside
	// the claim window the withdrawn head broke.
	appendStopMarker(t, ft.home, base.Add(20*time.Millisecond).UnixMilli())
	att.pollConfirmations()
	wantRefusedCompletion(t, refusedRecord(t, att, pr2Key()), "the answer")
}

// A normal assistant entry replaces API-error text, and a continuation row
// does not end a healthy turn. An API-error line mid-turn never supplies the
// result and never fails the run while a later answer or continuation exists.
// Review finding, 2026-11-25: an early return skipped the failure path and widened the user case.
func TestHealthyAssistantAndContinuationRemainUndisturbed(t *testing.T) {
	for _, row := range []struct {
		name  string
		lines func(t *testing.T) []string
	}{
		{name: "real answer after API error", lines: func(t *testing.T) []string {
			t.Helper()
			return []string{
				refusedAssistant(t, "part one", false),
				refusedAssistant(t, "API error", true),
				refusedAssistant(t, "the real answer", false),
			}
		}},
		{name: "continuation inside healthy turn", lines: func(t *testing.T) []string {
			t.Helper()
			return []string{
				refusedAssistant(t, "working", false),
				refusedLine(t, map[string]any{"type": "user", "isMeta": true, "origin": map[string]any{"kind": "auto-continuation"},
					"turnOrigin": "auto_continuation", "message": map[string]any{"role": "user", "content": "Your usage limit has reset. Continue the task."}}),
				refusedAssistant(t, "P1 done", false),
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ft := newFakeTarget(t, os.Getpid(), nil)
			att := ft.attach(t)
			events := &refusedEvents{}
			events.subscribe(att)
			admission := submitTurnRun(t, att, ft)
			appendTranscript(t, ft.home, row.lines(t)...)
			appendStopMarker(t, ft.home, time.Now().Add(2*time.Millisecond).UnixMilli())
			att.pollConfirmations()
			wantRefusedCompletion(t, refusedRecord(t, att, pr2Key()), map[string]string{
				"real answer after API error":      "the real answer",
				"continuation inside healthy turn": "P1 done",
			}[row.name])
			if len(events.terminals(admission.RunID)) != 1 {
				t.Fatalf("terminal events = %d, want exactly one completion", len(events.terminals(admission.RunID)))
			}
		})
	}
}
