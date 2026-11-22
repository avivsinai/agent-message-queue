package codex

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// askingMode is the thread/resume mode of a thread that asks: on-request,
// answered by the user, in the workspace-write sandbox.
const askingMode = `,"approvalPolicy":"on-request","sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false}`

// neverAsks is a thread/settings/updated that turns the approvals off.
const neverAsks = `{"threadId":"t1","threadSettings":{"approvalPolicy":"never","approvalsReviewer":"user","sandboxPolicy":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false}}}`

// setResumeMode sets the mode members thread/resume answers with and a hook
// that runs before the answer is written.
func (s *fakeAppServer) setResumeMode(mode string, hook func()) {
	s.resumeReviewerMu.Lock()
	defer s.resumeReviewerMu.Unlock()
	s.resumeMode, s.resumeHook = mode, hook
}

func attachWithMode(t *testing.T, srv *fakeAppServer, sock string) *Attachment {
	t.Helper()
	att, err := Attach(sock, "t1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	return att
}

func grantedSubmit(t *testing.T, att *Attachment, id string) core.Admission {
	t.Helper()
	s := att.Inspect()
	done := make(chan core.Admission, 1)
	go func() {
		adm, _ := att.Submit(core.BoundRequest{Key: requests.Key{CreatorHost: "link-aaaaaaaaaaaaaaaa", TargetID: s.TargetID, RequestID: id},
			Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "run the tests"}, Granted: true})
		done <- adm
	}()
	select {
	case adm := <-done:
		return adm
	case <-time.After(2 * time.Second):
		t.Fatal("granted submit did not answer: it sent a turn")
		return core.Admission{}
	}
}

// Contract section 5 (Codex): a granted request runs while the thread asks,
// as the attachment read it at thread/resume, with no override on turn/start.
func TestGrantedSubmitRunsWhileTheThreadAsks(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	srv.setResumeMode(askingMode, nil)
	att := attachWithMode(t, srv, sock)
	s := att.Inspect()
	key := requests.Key{CreatorHost: "link-aaaaaaaaaaaaaaaa", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111601"}
	done := make(chan core.Admission, 1)
	go func() {
		adm, _ := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "run the tests"}, Granted: true})
		done <- adm
	}()
	var call rpcMessage
	select {
	case call = <-srv.calls:
	case adm := <-done:
		t.Fatalf("granted submit = %+v before any native call; want turn/start", adm)
	}
	var params map[string]any
	_ = json.Unmarshal(call.Params, &params)
	if call.Method != "turn/start" || len(params) != 3 || params["threadId"] != "t1" || params["clientUserMessageId"] != clientIDFor(key) || params["input"] == nil {
		t.Fatalf("granted submit sent %s %v; want turn/start with only the thread, the input and the client message id", call.Method, params)
	}
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	select {
	case adm := <-done:
		if !adm.Admitted {
			t.Fatalf("granted submit = %+v, want admitted", adm)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("granted submit did not admit")
	}
}

// Contract section 5 (Codex): a thread/settings/updated replaces the mode, so
// a granted request after a change to "never" is refused with nothing sent.
func TestGrantedSubmitIsRefusedAfterTheThreadStopsAsking(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	srv.setResumeMode(askingMode, nil)
	att := attachWithMode(t, srv, sock)
	if asks, why := att.SessionAsks(); !asks {
		t.Fatalf("after resume: asks false (%s), want true", why)
	}
	srv.notify(t, "thread/settings/updated", neverAsks)
	deadline := time.Now().Add(5 * time.Second)
	for asks, _ := att.SessionAsks(); asks; asks, _ = att.SessionAsks() {
		if time.Now().After(deadline) {
			t.Fatal("the settings update to never was not applied")
		}
		runtime.Gosched()
	}
	adm := grantedSubmit(t, att, "11111111-1111-4111-8111-111111111602")
	if adm.Admitted || adm.Code != protocol.CodeConsentInvalid || !adm.NothingSent || !strings.HasPrefix(adm.Message, "grant_needs_signature: ") {
		t.Fatalf("granted submit after the change to never = %+v; want consent_invalid grant_needs_signature, nothing sent", adm)
	}
}

// Contract section 4: a session that can write the machine's state directory
// with no dialog takes no grant. The thread's working directory, a writable
// root of its sandbox, a runtime workspace root of the thread (from
// thread/resume only), or /tmp and $TMPDIR under workspaceWrite unless the
// policy excludes them, that holds the directory or lies inside it counts
// (review of the session grant branch: a granted task could have the session
// rewrite the grant state, also through the roots it missed).
func TestSessionThatWritesTheStateDirectory(t *testing.T) {
	work, elsewhere, runtimeRoot, tmp, outside := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	byCwd := filepath.Join(work, ".agent-mail", "extensions", "remote")
	byRoot := filepath.Join(elsewhere, "remote")
	byRuntime := filepath.Join(runtimeRoot, "remote")
	byTmpdir := filepath.Join(tmp, "amq", "remote")
	cwd, _ := json.Marshal(work)
	roots, _ := json.Marshal([]string{filepath.Join(byRoot, "link")})
	runtimeRoots, _ := json.Marshal([]string{runtimeRoot})
	sock, srv := startFakeAppServer(t)
	t.Setenv("TMPDIR", tmp) // after the socket, whose path stays short
	srv.setResumeMode(`,"cwd":`+string(cwd)+`,"runtimeWorkspaceRoots":`+string(runtimeRoots)+`,"approvalPolicy":"on-request","sandbox":{"type":"workspaceWrite","writableRoots":`+string(roots)+`,"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false}`, nil)
	att := attachWithMode(t, srv, sock)
	if asks, why := att.SessionAsks(); !asks {
		t.Fatalf("after resume: asks false (%s), want true", why)
	}
	want := map[string]bool{byCwd: true, byRoot: true, byRuntime: true, byTmpdir: true, outside: false}
	bySlashTmp := "/tmp/amq/remote"
	if filepath.Separator == '/' {
		want[bySlashTmp] = true
	}
	for dir, w := range want {
		if got := att.WritesWithoutAsking(dir); got != w {
			t.Errorf("after resume: WritesWithoutAsking(%s) = %v, want %v", dir, got, w)
		}
	}
	// The policy excludes both temporary directories; the runtime root stays.
	srv.notify(t, "thread/settings/updated", `{"threadId":"t1","threadSettings":{"cwd":`+string(cwd)+`,"approvalPolicy":"on-request","approvalsReviewer":"user","sandboxPolicy":{"type":"workspaceWrite","writableRoots":`+string(roots)+`,"networkAccess":false,"excludeTmpdirEnvVar":true,"excludeSlashTmp":true}}}`)
	deadline := time.Now().Add(5 * time.Second)
	for att.WritesWithoutAsking(byTmpdir) {
		if time.Now().After(deadline) {
			t.Fatal("the settings update that excludes $TMPDIR was not applied")
		}
		runtime.Gosched()
	}
	want[byTmpdir] = false
	if filepath.Separator == '/' {
		want[bySlashTmp] = false
	}
	for dir, w := range want {
		if got := att.WritesWithoutAsking(dir); got != w {
			t.Errorf("after the update: WritesWithoutAsking(%s) = %v, want %v", dir, got, w)
		}
	}
}

// Attack finding: a thread/settings/updated that arrives while thread/resume
// is in flight is newer than the resume answer, which must not overwrite it.
func TestSettingsUpdateBeforeTheResumeAnswerWins(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	srv.setResumeMode(askingMode, func() { srv.notify(t, "thread/settings/updated", neverAsks) })
	att := attachWithMode(t, srv, sock)
	if asks, why := att.SessionAsks(); asks || !strings.Contains(why, "approvalPolicy=never") {
		t.Fatalf("after an update before the resume answer: asks %v (%s), want false with approvalPolicy=never", asks, why)
	}
}
