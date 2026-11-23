package codex

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
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

// scopedSandbox is the workspace-write sandbox policy that writes roots and
// neither temporary directory, and the thread's working directory work, as
// JSON members.
func scopedSandbox(key, work string, roots ...string) string {
	cwd, _ := json.Marshal(work)
	writable, _ := json.Marshal(append([]string{}, roots...))
	return `"cwd":` + string(cwd) + `,"` + key + `":{"type":"workspaceWrite","writableRoots":` + string(writable) + `,"networkAccess":false,"excludeTmpdirEnvVar":true,"excludeSlashTmp":true}`
}

// scopedMode is the thread/resume mode of a thread that asks, works in work
// and writes only there and in roots.
func scopedMode(work string, roots ...string) string {
	return `,"approvalPolicy":"on-request",` + scopedSandbox("sandbox", work, roots...)
}

// scopedSettings is the thread/settings/updated of scopedMode.
func scopedSettings(work string, roots ...string) string {
	return `{"threadId":"t1","threadSettings":{"approvalPolicy":"on-request","approvalsReviewer":"user",` + scopedSandbox("sandboxPolicy", work, roots...) + `}}`
}

func attachWithMode(t *testing.T, srv *fakeAppServer, sock string, opts ...Option) *Attachment {
	t.Helper()
	att, err := Attach(sock, "t1", opts...)
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

// Contract section 5 (Codex): a granted request runs while the thread asks
// and cannot write the endpoint's state directory, as the attachment read it
// at thread/resume, with no override on turn/start.
func TestGrantedSubmitRunsWhileTheThreadAsks(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	sock, srv := startFakeAppServer(t)
	srv.setResumeMode(scopedMode(work), nil)
	att := attachWithMode(t, srv, sock, WithStateDir(state))
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

// Contract section 4: the last check of a granted request is the adapter's,
// on the settings of the dispatch itself. A thread/settings/updated after the
// admission check that adds the state directory to the writable roots, while
// the policy still asks, refuses the granted request with nothing sent
// (review of the session grant branch: only grant_info and the admission
// checked the directory, on settings that could change before the dispatch).
func TestGrantedSubmitIsRefusedWhenTheThreadCanWriteTheState(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	sock, srv := startFakeAppServer(t)
	srv.setResumeMode(scopedMode(work), nil)
	att := attachWithMode(t, srv, sock, WithStateDir(state))
	if att.WritesWithoutAsking(state) {
		t.Fatal("after resume: the thread writes the state directory; want not, as the admission check saw")
	}
	srv.notify(t, "thread/settings/updated", scopedSettings(work, state))
	deadline := time.Now().Add(5 * time.Second)
	for !att.WritesWithoutAsking(state) {
		if time.Now().After(deadline) {
			t.Fatal("the settings update that adds the state directory was not applied")
		}
		runtime.Gosched()
	}
	if asks, why := att.SessionAsks(); !asks {
		t.Fatalf("after the update: asks false (%s), want true", why)
	}
	adm := grantedSubmit(t, att, "11111111-1111-4111-8111-111111111603")
	const want = "grant_unsupported: state_in_workspace: the session can write this machine's state with no dialog"
	if adm.Admitted || adm.Code != protocol.CodeConsentInvalid || !adm.NothingSent || adm.Message != want {
		t.Fatalf("granted submit after the update = %+v; want consent_invalid %q, nothing sent", adm, want)
	}
	select {
	case call := <-srv.calls:
		t.Fatalf("the refused granted submit sent %s", call.Method)
	default:
	}
}

// The adapter's last check needs the endpoint's state directory. An
// attachment built with none refuses every granted request, and the factory
// passes the build call's directory (merge check of the session grant
// branch: without the second, every granted Codex task is refused in
// production while every other test passes).
func TestGrantedSubmitNeedsTheStateDirectoryOfItsBuild(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	const refused = "grant_unsupported: state_in_workspace: the session can write this machine's state with no dialog"

	sock, srv := startFakeAppServer(t)
	srv.setResumeMode(scopedMode(work), nil)
	bare := attachWithMode(t, srv, sock)
	if adm := grantedSubmit(t, bare, "11111111-1111-4111-8111-111111111604"); adm.Admitted || adm.Code != protocol.CodeConsentInvalid || !adm.NothingSent || adm.Message != refused {
		t.Fatalf("granted submit with no state directory = %+v; want consent_invalid %q, nothing sent", adm, refused)
	}

	sock, srv = startFakeAppServer(t)
	srv.setResumeMode(scopedMode(work), nil)
	conf, _ := json.Marshal(config{Socket: sock, Thread: "t1"})
	built, err := Factory(context.Background(), registry.FactoryConfig{StateDir: state, Config: conf})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	att := built.(*Attachment)
	t.Cleanup(func() { _ = att.Close() })
	if want, _ := filepath.Abs(state); att.stateDir != want {
		t.Fatalf("the factory's attachment holds state directory %q, want %q", att.stateDir, want)
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
	// With /tmp writable, a test directory under /tmp is written too, so
	// outside is checked only once the policy excludes /tmp.
	want := map[string]bool{byCwd: true, byRoot: true, byRuntime: true, byTmpdir: true}
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
	want[byTmpdir], want[outside] = false, false
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
