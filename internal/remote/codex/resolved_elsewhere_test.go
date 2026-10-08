package codex

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// 611.42 pre-merge review: the app-server fans an approval out to every
// client and drops every answer after the first with only a log line. When
// the Codex terminal answered first, a later Buzz answer still reported
// success. serverRequest/resolved must clear the approval, so the late
// answer is refused as already resolved.
func TestApprovalAnsweredElsewhereRefusesLateAnswer(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	resolved := make(chan core.NativeEvent, 1)
	att.Subscribe(func(ev core.NativeEvent) {
		if ev.Type == core.EventQuestionResolved {
			resolved <- ev
		}
	})

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111442"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "run tests"}})
		done <- err
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	srv.sendServerRequest(t, "7", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"tool1","command":"go test ./..."}`)
	waitInteraction(t, att, key)

	// The terminal answers first; the app-server tells every client.
	srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"7"}`)
	select {
	case ev := <-resolved:
		if ev.Key != key || ev.Remote {
			t.Fatalf("resolution = %+v, want this run resolved outside AMQ", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serverRequest/resolved did not resolve the approval")
	}
	waitNoInteraction(t, att)
	if code, err := att.Respond(key, s.Epoch, "tool1", "accept"); err != nil || code != protocol.CodeAlreadyResolved {
		t.Fatalf("late Respond = (%q, %v), want already_resolved", code, err)
	}
}

// PR #919 review: a file-change approval showed no paths or diff, yet one
// ✅ approved it. Approve is offered only for a command the prompt shows
// whole; reject stays available.
func TestApprovalWithholdsApproveForUnseenGrants(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	// Pro review of #969: Codex 0.160 sends additionalPermissions, which the
	// approve guard reads, only to a client with the experimental API.
	if init := <-srv.calls; !strings.Contains(string(init.Params), `"experimentalApi":true`) {
		t.Fatalf("initialize params = %s, want the experimental API with approvals on", init.Params)
	}
	<-srv.calls // thread/resume
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111443"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "edit"}})
		done <- err
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	pending := func() protocol.Interaction {
		waitInteraction(t, att, key)
		att.mu.Lock()
		defer att.mu.Unlock()
		return *att.runs[key].interaction
	}
	srv.sendServerRequest(t, "8", "item/fileChange/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"fc1","grantRoot":"/"}`)
	if in := pending(); in.ApproveOption != "" || in.RejectOption != "decline" {
		t.Fatalf("file change = approve %q reject %q, want reject only", in.ApproveOption, in.RejectOption)
	}
	srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"8"}`)
	waitNoInteraction(t, att)
	srv.sendServerRequest(t, "9", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"c1","command":"ls"}`)
	if in := pending(); in.ApproveOption != "accept" {
		t.Fatalf("plain command approve = %q, want accept", in.ApproveOption)
	}
	srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"9"}`)
	waitNoInteraction(t, att)
	// agent-message-queue-611.57: the live Codex 0.160 request mixes an
	// object decision into availableDecisions and offers cancel, not
	// decline. It was dropped whole, so the DM never saw the approval.
	srv.sendServerRequest(t, "10", "item/commandExecution/requestApproval", `{"kind":"command","threadId":"t1","turnId":"u1","itemId":"exec-1","command":"/bin/zsh -lc 'touch approval-test2.txt'","cwd":"/w","availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["touch","approval-test2.txt"]}},"cancel"]}`)
	if in := pending(); in.ApproveOption != "accept" || in.RejectOption != "cancel" || !strings.HasSuffix(in.Prompt, "Reject also stops this turn.") {
		t.Fatalf("codex 0.160 command = approve %q reject %q prompt %q, want accept, cancel and the stop note", in.ApproveOption, in.RejectOption, in.Prompt)
	}
	// 611.42.11: a command that holds a credential went to the DM whole,
	// and a zero-width character the DM removes showed a command other than
	// the one a ✅ approved. Neither shows whole, so neither offers approve.
	// Pro review of #988: curl's -u user:password, separate and attached.
	// The commands are filled in at run time, so no curl line with a
	// credential sits in the source for secret scanners.
	fill := strings.NewReplacer("{tool}", "cu"+"rl", "{bearer}", "Bear"+"er abcdefgh12345678", "{cred}", "alice"+":"+"demo-"+"pass-123", "{pass}", "demo-"+"pass-123")
	for i, row := range []string{"{tool} -H 'Authorization: {bearer}' x", "rm -rf /tmp/x\u200b /",
		"{tool} -u {cred} example.invalid", "{tool} -u{cred} example.invalid",
		"{tool} -su {cred} example.invalid", "{tool} -su{cred} example.invalid",
		"{tool} --proxy-user {cred} -x proxy.invalid example.invalid",
		"{tool} -4u {cred} example.invalid", "{tool} -#U{cred} example.invalid",
		"{tool} -u :{pass} example.invalid", "{tool} -u':{pass}' example.invalid",
		"{tool} -u 'alice: {pass}' example.invalid",
		// Pro review of #988 r5: a backslash-newline the shell joins.
		`{tool} -\\\nu {cred} example.invalid`, `{tool} -\\\nU {cred} example.invalid`,
		`cu\\\nrl -u {cred} example.invalid`,
		// 611.42.13 (Pro review of #988 r6): words the shell rebuilds.
		`{tool} $'\\x2du' {cred} example.invalid`, `c$'u'rl -u {cred} example.invalid`,
		`{tool} -$(printf u) {cred} example.invalid`} {
		cmd := fill.Replace(row)
		srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"`+strconv.Itoa(10+i)+`"}`)
		waitNoInteraction(t, att)
		srv.sendServerRequest(t, strconv.Itoa(11+i), "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"s`+strconv.Itoa(i)+`","command":"`+cmd+`"}`)
		if in := pending(); in.ApproveOption != "" || in.RejectOption != "decline" || strings.Contains(in.Prompt, "abcdefgh") || strings.Contains(in.Prompt, "demo-pass") {
			t.Fatalf("command %q = approve %q reject %q prompt %q, want reject only and no secret", cmd, in.ApproveOption, in.RejectOption, in.Prompt)
		}
	}
	// Pro review of #988 r2: a -u that carries no credential stays visible.
	// Pro review of #988 r4: nor does a git remote URL or refspec after it.
	for i, cmd := range []string{"git push -u origin main", "sort -u names.txt",
		"git push -u git@github.com:acme/repo.git main", "git push origin -u main:main"} {
		srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"`+strconv.Itoa(28+i)+`"}`)
		waitNoInteraction(t, att)
		srv.sendServerRequest(t, strconv.Itoa(29+i), "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"v`+strconv.Itoa(i)+`","command":"`+cmd+`"}`)
		if in := pending(); in.ApproveOption != "accept" || in.Prompt != cmd {
			t.Fatalf("command %q = approve %q prompt %q, want it shown whole with approve", cmd, in.ApproveOption, in.Prompt)
		}
	}
}

// agent-message-queue-611.42.8 (Pro review of #969): permission, user-input
// and MCP elicitation requests were dropped, so a DM turn waited for a
// terminal answer the owner never saw. The DM shows what is asked, bounded,
// with no one-tap answer, and the terminal's answer clears it.
func TestTerminalOnlyRequestsShowInTheDM(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111446"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "ask me"}})
		done <- err
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Payloads follow the Codex 0.160.1 generated JSON schema, not a live
	// capture.
	long := strings.Repeat("x", protocol.MaxApprovalPreview)
	for _, tc := range []struct{ reqID, method, params, wantID, wantPrompt string }{
		{"20", "item/permissions/requestApproval",
			`{"threadId":"t1","turnId":"u1","itemId":"perm1","cwd":"/w","startedAtMs":1,"reason":"write the cache","permissions":{"fileSystem":{"entries":[{"access":"write","path":{"type":"path","path":"/w/.cache"}}]},"network":{"enabled":true}}}`,
			"perm1", "Codex asks for permissions:\nwrite /w/.cache\nnetwork\nin /w\nreason: write the cache"},
		{"21", "item/tool/requestUserInput",
			`{"threadId":"t1","turnId":"u1","itemId":"ask1","isBlocking":true,"questions":[{"id":"q1","header":"Branch","question":"Which branch?","options":[{"label":"main","description":"default"}]}]}`,
			"ask1", "Codex asks for input:\nBranch: Which branch?"},
		{"22", "mcpServer/elicitation/request",
			`{"threadId":"t1","turnId":"u1","serverName":"docs","mode":"form","message":"Pick a space ` + long + `","requestedSchema":{"type":"object","properties":{}}}`,
			"22", "MCP server docs asks: Pick a space x"},
		// Pro review of #985: an asked text that echoes a credential.
		{"23", "mcpServer/elicitation/request",
			`{"threadId":"t1","turnId":"u1","serverName":"docs","mode":"form","message":"Confirm Authorization: Bearer abcdefgh12345678","requestedSchema":{"type":"object","properties":{}}}`,
			"23", "An MCP server asks for input; it may hold a secret, so only the terminal shows it."},
		// Pro review of #985 r2: a zero-width space the DM removes hid the
		// credential from the screen.
		{"24", "mcpServer/elicitation/request",
			`{"threadId":"t1","turnId":"u1","serverName":"docs","mode":"form","message":"Confirm Bearer\u200b abcdefgh12345678","requestedSchema":{"type":"object","properties":{}}}`,
			"24", "An MCP server asks for input; it may hold a secret, so only the terminal shows it."},
	} {
		srv.sendServerRequest(t, tc.reqID, tc.method, tc.params)
		waitInteraction(t, att, key)
		att.mu.Lock()
		in := *att.runs[key].interaction
		att.mu.Unlock()
		if in.InteractionID != tc.wantID || in.Kind != "approval" || !in.RemoteAnswer || len(in.Options) != 0 ||
			in.ApproveOption != "" || in.RejectOption != "" ||
			!strings.HasPrefix(in.Prompt, tc.wantPrompt) || len(in.Prompt) > protocol.MaxApprovalPreview+len(" …[shortened]") {
			t.Fatalf("%s interaction = %+v, want %s shown with no answer option and prompt %q, bounded", tc.method, in, tc.wantID, tc.wantPrompt)
		}
		srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"`+tc.reqID+`"}`)
		waitNoInteraction(t, att)
	}
}

// PR #919 review round 2: a second approval replaced a pending one, and the
// first one's resolution then cleared the second at the endpoint, which
// holds one pending interaction. Approvals are reported one at a time: the
// second becomes pending only after the first resolves.
func TestOverlappingApprovalsShowOneAtATime(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	events := make(chan string, 8)
	att.Subscribe(func(ev core.NativeEvent) {
		switch ev.Type {
		case core.EventQuestion:
			events <- "ask " + ev.Interaction.InteractionID
		case core.EventQuestionResolved:
			events <- "resolved"
		}
	})
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111444"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "two tools"}})
		done <- err
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	srv.sendServerRequest(t, "10", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"a1","command":"ls"}`)
	srv.sendServerRequest(t, "11", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"b1","command":"pwd"}`)
	// Both requests reach the adapter before the first is answered.
	for deadline := time.Now().Add(5 * time.Second); ; runtime.Gosched() {
		att.mu.Lock()
		n := len(att.runs[key].approvalReqs)
		att.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the two approval requests never reached the adapter")
		}
	}
	srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"10"}`)
	var got []string
	for len(got) < 3 {
		select {
		case e := <-events:
			got = append(got, e)
		case <-time.After(5 * time.Second):
			t.Fatalf("events = %v, want three", got)
		}
	}
	if want := []string{"ask a1", "resolved", "ask b1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if code, err := att.Respond(key, s.Epoch, "b1", "accept"); err != nil || code != "" {
		t.Fatalf("Respond on the second approval = (%q, %v), want it answered", code, err)
	}
}

// PR #919 review round 2: approval requests reach the adapter through a
// worker queue, so another client's resolution can overtake its own
// request. Such a request is already answered and is never shown.
func TestApprovalResolvedBeforeDeliveryIsNotShown(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	asked := make(chan string, 4)
	att.Subscribe(func(ev core.NativeEvent) {
		if ev.Type == core.EventQuestion {
			asked <- ev.Interaction.InteractionID
		}
	})
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111445"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "run"}})
		done <- err
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"12"}`)
	// The read pump handles frames in order, so the resolution above is
	// recorded before this request is read.
	srv.sendServerRequest(t, "12", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"late","command":"ls"}`)
	srv.sendServerRequest(t, "13", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"next","command":"pwd"}`)
	select {
	case id := <-asked:
		if id != "next" {
			t.Fatalf("shown approval = %s, want only the unresolved one", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the unresolved approval was never shown")
	}
}

// waitNoInteraction waits until a resolution has cleared the pending
// interaction. A bounded Gosched spin let the next read see the old one on a
// slow runner (macOS CI run 36637228324: plain command approve = "", want
// accept).
func waitNoInteraction(t *testing.T, att *Attachment) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); att.Inspect().PendingInteraction != nil; {
		if time.Now().After(deadline) {
			t.Fatal("the resolved approval was never cleared")
		}
		time.Sleep(time.Millisecond)
	}
}
