package codex

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// A request of a turn started in the Codex TUI opens a dialog for the
// binding that serves the thread, with Codex's own decisions; the owner's
// answer reaches the app-server as Codex's response shape.
func TestTUIRequestsOpenDialogsAndReplyWithTheAnswer(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(base, "remote", "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Name: "cx", Root: base, Target: "codex:t1", NativeSession: "t1"}); err != nil {
		t.Fatal(err)
	}
	dir, err := dialog.DirFor("cx")
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

	opened := func(after string) dialog.Request {
		t.Helper()
		for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			open, _ := dialog.Requests(dir)
			for _, r := range open {
				if r.ID != after {
					return r
				}
			}
		}
		t.Fatal("no dialog opened")
		return dialog.Request{}
	}
	reply := func(id string) string {
		t.Helper()
		for {
			select {
			case msg := <-srv.calls:
				if msg.Method == "" && msg.ID != nil && string(*msg.ID) == `"`+id+`"` {
					return string(msg.Result)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("no response to request %s", id)
			}
		}
	}

	srv.sendServerRequest(t, "7", methodCommandApproval, `{"threadId":"t1","turnId":"tui-1","itemId":"c1","command":"go test ./...","cwd":"/w",
		"availableDecisions":["accept","acceptForSession","decline"]}`)
	req := opened("")
	if req.Tool != "exec" || req.Text != "$ go test ./...\nin /w" || len(req.Options) != 3 ||
		req.Options[1].Value != "acceptForSession" || req.Options[1].Label != "Allow for session" || !req.Options[2].Deny {
		t.Fatalf("dialog = %+v, want the command with Codex's three decisions", req)
	}
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: req.ID, Option: 2, By: dialog.ByBuzz}); err != nil {
		t.Fatal(err)
	}
	if got := reply("7"); got != `{"decision":"acceptForSession"}` {
		t.Fatalf("response = %s, want acceptForSession", got)
	}

	srv.sendServerRequest(t, "8", methodToolUserInput, `{"threadId":"t1","turnId":"tui-1","itemId":"u1","isBlocking":true,
		"questions":[{"id":"branch","header":"Branch","question":"Which branch?"}]}`)
	req = opened(req.ID)
	if !req.FreeText || req.Text != "Branch: Which branch?" {
		t.Fatalf("dialog = %+v, want the free-text question", req)
	}
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: req.ID, Text: "main", By: dialog.ByBuzz}); err != nil {
		t.Fatal(err)
	}
	if got := reply("8"); got != `{"answers":{"branch":{"answers":["main"]}}}` {
		t.Fatalf("response = %s, want the typed answer", got)
	}
	r, ok, _ := dialog.ReadResolved(dir, req.ID)
	for deadline := time.Now().Add(4 * time.Second); !ok && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r, ok, _ = dialog.ReadResolved(dir, req.ID)
	}
	if r.Outcome != dialog.OutcomeAnswered || r.By != dialog.ByBuzz {
		t.Fatalf("resolved = %+v, want answered from Buzz", r)
	}
}

// A thread no binding serves still mirrors its dialogs, to the machine
// agent, named by the session's project.
func TestUnboundThreadOpensMachineDialog(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(base, "remote", "binding.json"))
	dir, err := dialog.DirFor(dialog.MachineBinding)
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

	srv.sendServerRequest(t, "7", methodCommandApproval, `{"threadId":"t1","turnId":"tui-1","itemId":"c1","command":"ls"}`)
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if open, _ := dialog.Requests(dir); len(open) == 1 {
			if open[0].Label != "codex · work" || open[0].Text != "$ ls" {
				t.Fatalf("dialog = %+v, want the machine dialog of codex · work", open[0])
			}
			return
		}
	}
	t.Fatal("no machine dialog opened")
}
