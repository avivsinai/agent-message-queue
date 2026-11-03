package codex

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// dialogHarness is an attachment on a fake app-server whose binding store is
// a temp dir; bound names the binding that serves thread t1.
type dialogHarness struct {
	t   *testing.T
	att *Attachment
	srv *fakeAppServer
	dir string
}

func newDialogHarness(t *testing.T, bound string) *dialogHarness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(base, "remote", "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Name: bound, Root: base, Target: "codex:t1", NativeSession: "t1"}); err != nil {
		t.Fatal(err)
	}
	dir, err := dialog.DirFor(bound)
	if err != nil {
		t.Fatal(err)
	}
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	return &dialogHarness{t: t, att: att, srv: srv, dir: dir}
}

// opened waits for an open dialog other than after.
func (h *dialogHarness) opened(after string) dialog.Request {
	h.t.Helper()
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		open, _ := dialog.Requests(h.dir)
		for _, r := range open {
			if r.ID != after {
				return r
			}
		}
	}
	h.t.Fatal("no dialog opened")
	return dialog.Request{}
}

// answer writes the owner's answer, waiting while an earlier one is there.
func (h *dialogHarness) answer(a dialog.Answer) {
	h.t.Helper()
	a.By = dialog.ByBuzz
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok, err := dialog.WriteAnswer(h.dir, a); err != nil {
			h.t.Fatal(err)
		} else if ok {
			return
		}
	}
	h.t.Fatalf("answer to %s never written", a.ID)
}

// reply waits for the attachment's response to server request id.
func (h *dialogHarness) reply(id string) string {
	h.t.Helper()
	for {
		select {
		case msg := <-h.srv.calls:
			if msg.Method == "" && msg.ID != nil && string(*msg.ID) == `"`+id+`"` {
				return string(msg.Result)
			}
		case <-time.After(4 * time.Second):
			h.t.Fatalf("no response to request %s", id)
		}
	}
}

// A request of a turn started in the Codex TUI opens a dialog for the
// binding that serves the thread, with Codex's own decisions; the owner's
// answer reaches the app-server as Codex's response shape.
func TestTUIRequestsOpenDialogsAndReplyWithTheAnswer(t *testing.T) {
	h := newDialogHarness(t, "cx")
	h.srv.sendServerRequest(t, "7", methodCommandApproval, `{"threadId":"t1","turnId":"tui-1","itemId":"c1","command":"go test ./...","cwd":"/w",
		"availableDecisions":["accept","acceptForSession","decline"]}`)
	req := h.opened("")
	if req.Tool != "exec" || req.Text != "$ go test ./...\nin /w" || len(req.Options) != 3 ||
		req.Options[1].Value != "acceptForSession" || req.Options[1].Label != "Allow for session" || !req.Options[2].Deny {
		t.Fatalf("dialog = %+v, want the command with Codex's three decisions", req)
	}
	h.answer(dialog.Answer{ID: req.ID, Option: 2})
	if got := h.reply("7"); got != `{"decision":"acceptForSession"}` {
		t.Fatalf("response = %s, want acceptForSession", got)
	}

	h.srv.sendServerRequest(t, "8", methodToolUserInput, `{"threadId":"t1","turnId":"tui-1","itemId":"u1","isBlocking":true,
		"questions":[{"id":"branch","header":"Branch","question":"Which branch?"}]}`)
	req = h.opened(req.ID)
	if !req.FreeText || req.Text != "Branch: Which branch?" {
		t.Fatalf("dialog = %+v, want the free-text question", req)
	}
	h.answer(dialog.Answer{ID: req.ID, Text: "main"})
	if got := h.reply("8"); got != `{"answers":{"branch":{"answers":["main"]}}}` {
		t.Fatalf("response = %s, want the typed answer", got)
	}
	r, ok, _ := dialog.ReadResolved(h.dir, req.ID)
	for deadline := time.Now().Add(4 * time.Second); !ok && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r, ok, _ = dialog.ReadResolved(h.dir, req.ID)
	}
	if r.Outcome != dialog.OutcomeAnswered || r.By != dialog.ByBuzz {
		t.Fatalf("resolved = %+v, want answered from Buzz", r)
	}
}

// Regression (review of the dialog attachment, finding 1): an answer that
// fits no choice stayed in answers/<id>.json forever, so the owner's
// corrected reply could never be written and the dialog hung.
func TestRejectedAnswerLetsTheOwnerAnswerAgain(t *testing.T) {
	h := newDialogHarness(t, "cx")
	h.srv.sendServerRequest(t, "9", methodMcpElicitation, `{"threadId":"t1","turnId":"tui-1","serverName":"db","mode":"form","message":"Log in",
		"requestedSchema":{"type":"object","properties":{"user":{"type":"string"},"pass":{"type":"string"}}}}`)
	req := h.opened("")
	h.answer(dialog.Answer{ID: req.ID, Text: "alice"}) // two fields: plain text fits neither
	h.answer(dialog.Answer{ID: req.ID, Text: `{"user":"alice","pass":"x"}`})
	if got := h.reply("9"); got != `{"action":"accept","content":{"pass":"x","user":"alice"}}` {
		t.Fatalf("response = %s, want the corrected answer", got)
	}
}

// Regression (review of the dialog attachment, finding 2): after our own
// Buzz answer closed a dialog, its serverRequest/resolved was recorded as an
// early resolution, so a later request that reused the JSON-RPC id was never
// shown in Buzz.
func TestReusedRequestIDAfterOwnAnswerIsShown(t *testing.T) {
	h := newDialogHarness(t, "cx")
	h.srv.sendServerRequest(t, "7", methodCommandApproval, `{"threadId":"t1","turnId":"tui-1","itemId":"c1","command":"ls"}`)
	first := h.opened("")
	h.answer(dialog.Answer{ID: first.ID, Option: 1})
	if got := h.reply("7"); got != `{"decision":"accept"}` {
		t.Fatalf("response = %s, want accept", got)
	}
	for deadline := time.Now().Add(4 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		h.att.mu.Lock()
		open := len(h.att.dialogs)
		h.att.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the answered dialog never closed")
		}
	}
	h.srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"7"}`)
	h.srv.sendServerRequest(t, "7", methodCommandApproval, `{"threadId":"t1","turnId":"tui-2","itemId":"c2","command":"pwd"}`)
	if req := h.opened(first.ID); req.Text != "$ pwd" {
		t.Fatalf("dialog = %+v, want the request that reused id 7", req)
	}
}
