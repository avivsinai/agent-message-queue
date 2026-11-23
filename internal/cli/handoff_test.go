package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const handoffTestCommit = "0123456789abcdef0123456789abcdef01234567"

// handoffFixture is a queue where alice has sent bob an open todo.
func handoffFixture(t *testing.T) (root, request string) {
	t.Helper()
	clearSendMailboxTestEnv(t)
	root = filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "alice,bob,carol,dave"}); err != nil {
		t.Fatal(err)
	}
	return root, handoffSend(t, root, "alice", "bob", "todo", "build the parser")
}

func handoffSend(t *testing.T, root, from, to, kind, subject string) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", from, "--to", to, "--kind", kind, "--subject", subject, "--body", subject, "--json"})
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var sent struct{ ID string }
	if err := json.Unmarshal([]byte(out), &sent); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return sent.ID
}

func runHandoffAs(t *testing.T, root, me, request, reviewer string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"--root", root, "--me", me, "--id", request, "--to", reviewer, "--commit", handoffTestCommit, "--body", "ready for review"}, extra...)
	var err error
	stdout, _ := captureOutput(t, func() error {
		err = runHandoff(args)
		return err
	})
	return stdout, err
}

func inboxMessages(t *testing.T, root, agent string) []format.Message {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fsq.AgentInboxNew(root, agent), "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []format.Message
	for _, path := range paths {
		msg, err := format.ReadMessageFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, msg)
	}
	return out
}

func handoffCtx(msg format.Message) map[string]any {
	ctx, _ := msg.Header.Context["handoff"].(map[string]any)
	return ctx
}

// Observed: a builder finished and told only its reviewer; the requester's
// request stayed open and quiet. One hand-off tells both.
func TestHandoffSendsReviewAndTellsRequester(t *testing.T) {
	root, request := handoffFixture(t)
	stdout, err := runHandoffAs(t, root, "bob", request, "carol", "--json")
	if err != nil {
		t.Fatalf("handoff: %v\n%s", err, stdout)
	}
	var report handoffReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if report.Review.State != handoffSent || report.Notice.State != handoffSent || report.Review.ID == "" {
		t.Fatalf("report = %+v, want both legs sent", report)
	}

	reviews := inboxMessages(t, root, "carol")
	if len(reviews) != 1 || reviews[0].Header.Kind != format.KindReviewRequest || reviews[0].Header.ID != report.Review.ID {
		t.Fatalf("carol's inbox = %+v, want one review request %s", reviews, report.Review.ID)
	}
	if ctx := handoffCtx(reviews[0]); ctx["commit"] != handoffTestCommit || ctx["request"] != request || ctx["requester"] != "alice" {
		t.Fatalf("review context = %v", reviews[0].Header.Context)
	}

	notices := inboxMessages(t, root, "alice")
	if len(notices) != 1 {
		t.Fatalf("alice's inbox holds %d messages, want one notice", len(notices))
	}
	notice := notices[0]
	wantLine := "Handed to carol for review: " + handoffTestCommit + ". Review request " + report.Review.ID + "."
	if notice.Header.Kind != format.KindStatus || !slices.Contains(notice.Header.Labels, handedOffLabel) ||
		!slices.Contains(notice.Header.Refs, request) || !strings.HasPrefix(notice.Body, wantLine+"\n\nready for review") {
		t.Fatalf("notice = %+v", notice)
	}
	if ctx := handoffCtx(notice); ctx["review"] != report.Review.ID || ctx["reviewer"] != "carol" || ctx["commit"] != handoffTestCommit {
		t.Fatalf("notice context = %v", notice.Header.Context)
	}

	stdout, stderr := captureOutput(t, func() error { return runThread([]string{"open", "--json", "--root", root}) })
	var open threadOpenResult
	if err := json.Unmarshal([]byte(stdout), &open); err != nil {
		t.Fatalf("thread open = %s (err %v) %s", stdout, err, stderr)
	}
	found := false
	for _, ob := range open.Obligations {
		if ob.Opener.ID == request {
			found = true
			if ob.State != thread.StateAcknowledged {
				t.Fatalf("alice's request: state %q after the notice, want it acknowledged", ob.State)
			}
		}
	}
	if !found {
		t.Fatalf("alice's request is not open after the hand-off: %s", stdout)
	}
}

func TestHandoffRefusalsAndReruns(t *testing.T) {
	count := func(t *testing.T, root, agent string) int { return len(inboxMessages(t, root, agent)) }
	// nothingSent checks err names want (unless want is empty) and that no
	// review request and no hand-off notice was delivered.
	nothingSent := func(t *testing.T, root string, err error, want string) {
		t.Helper()
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if n := count(t, root, "carol"); n != 0 {
			t.Fatalf("carol holds %d messages, want none", n)
		}
		for _, msg := range inboxMessages(t, root, "alice") {
			if slices.Contains(msg.Header.Labels, handedOffLabel) {
				t.Fatalf("alice holds a hand-off notice %s", msg.Header.ID)
			}
		}
	}
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a rerun after both legs sends nothing", func(t *testing.T) {
			root, request := handoffFixture(t)
			if _, err := runHandoffAs(t, root, "bob", request, "carol"); err != nil {
				t.Fatal(err)
			}
			out, err := runHandoffAs(t, root, "bob", request, "carol")
			if err != nil || strings.Count(out, ": already sent\n") != 2 || strings.Contains(out, handoffTextStands) {
				t.Fatalf("rerun = %q (err %v), want already sent twice", out, err)
			}
			out, err = runHandoffAs(t, root, "bob", request, "carol", "--body", "other words")
			if err != nil || strings.Count(out, ": already sent; "+handoffTextStands) != 2 {
				t.Fatalf("rerun with another body = %q (err %v), want the earlier text to stand", out, err)
			}
			if count(t, root, "carol") != 1 || count(t, root, "alice") != 1 {
				t.Fatal("a rerun sent a message again")
			}
		}},
		{"a run that stopped after the review is completed by the rerun", func(t *testing.T) {
			root, request := handoffFixture(t)
			if _, err := runHandoffAs(t, root, "bob", request, "carol"); err != nil {
				t.Fatal(err)
			}
			// Leave the state of a run that stopped between the legs: the
			// review is sent, the notice is not.
			notice := inboxMessages(t, root, "alice")[0].Header.ID
			for _, path := range []string{
				filepath.Join(fsq.AgentInboxNew(root, "alice"), notice+".md"),
				filepath.Join(fsq.AgentOutboxSent(root, "bob"), notice+".md"),
			} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			review := inboxMessages(t, root, "carol")[0].Header.ID
			out, err := runHandoffAs(t, root, "bob", request, "carol")
			if err != nil || !strings.Contains(out, "Review request "+review+" to carol: already sent\n") || !strings.Contains(out, " to alice: sent\n") {
				t.Fatalf("rerun = %q (err %v)", out, err)
			}
			notices := inboxMessages(t, root, "alice")
			if count(t, root, "carol") != 1 || len(notices) != 1 || handoffCtx(notices[0])["review"] != review {
				t.Fatalf("rerun: want one review and one notice naming %s", review)
			}
		}},
		{"the requester cannot be reached: nothing is sent", func(t *testing.T) {
			// A request routed from a session that does not exist.
			clearSendMailboxTestEnv(t)
			root := filepath.Join(t.TempDir(), ".agent-mail", "collab")
			if err := runInit([]string{"--root", root, "--agents", "alice,bob,carol"}); err != nil {
				t.Fatal(err)
			}
			routed := deliverOriginalForReply(t, root, "bob", format.Header{
				From: "alice", To: []string{"bob"}, Thread: "p2p/ghost:alice__collab:bob", Subject: "routed",
				Kind: format.KindTodo, ReplyTo: "alice@ghost",
			})
			out, err := runHandoffAs(t, root, "bob", routed, "carol")
			if GetExitCode(err) != 3 || out != "" {
				t.Fatalf("handoff = %q (err %v, exit %d), want the reply's not-found refusal and no report", out, err, GetExitCode(err))
			}
			nothingSent(t, root, err, `session "ghost" not found`)

			// A requester's mailbox the queue refuses to write to.
			if runtime.GOOS == "windows" {
				return
			}
			root, request := handoffFixture(t)
			tmp := fsq.AgentInboxTmp(root, "alice")
			if err := os.Chmod(tmp, 0o500); err != nil {
				t.Fatal(err)
			}
			_, err = runHandoffAs(t, root, "bob", request, "carol")
			if chmodErr := os.Chmod(tmp, 0o700); chmodErr != nil {
				t.Fatal(chmodErr)
			}
			nothingSent(t, root, err, "mailbox for \"alice\" is incomplete")
		}},
		// Independent check of the hand-off: with the caller's outbox not
		// writable the run exited 0, and the next run sent both legs again.
		{"a leg that is not recorded in the outbox fails the run", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				return
			}
			root, request := handoffFixture(t)
			sent := fsq.AgentOutboxSent(root, "bob")
			if err := os.Chmod(sent, 0o500); err != nil {
				t.Fatal(err)
			}
			out, err := runHandoffAs(t, root, "bob", request, "carol")
			if chmodErr := os.Chmod(sent, 0o700); chmodErr != nil {
				t.Fatal(chmodErr)
			}
			if GetExitCode(err) != 1 || strings.Count(out, handoffUnrecorded) != 2 || strings.Contains(out, "run the same command again") {
				t.Fatalf("handoff = %q (err %v), want exit 1 and both legs reported as not recorded", out, err)
			}
			if count(t, root, "carol") != 1 || count(t, root, "alice") != 1 {
				t.Fatal("want both legs delivered once")
			}
		}},
		{"a request the caller sent", func(t *testing.T) {
			root, _ := handoffFixture(t)
			mine := handoffSend(t, root, "bob", "dave", "todo", "mine")
			_, err := runHandoffAs(t, root, "bob", mine, "carol")
			nothingSent(t, root, err, "you sent this request; a hand-off is for a request you owe")
		}},
		{"a closed request", func(t *testing.T) {
			root, request := handoffFixture(t)
			if _, err := captureStdout(t, func() error {
				return runReply([]string{"--root", root, "--me", "bob", "--id", request, "--labels", "done", "--body", "done"})
			}); err != nil {
				t.Fatal(err)
			}
			_, err := runHandoffAs(t, root, "bob", request, "carol")
			nothingSent(t, root, err, "is closed")
		}},
		{"a plain status", func(t *testing.T) {
			root, _ := handoffFixture(t)
			status := handoffSend(t, root, "alice", "bob", "status", "fyi")
			_, err := runHandoffAs(t, root, "bob", status, "carol")
			nothingSent(t, root, err, "is not a request")
		}},
		{"the reviewer is the caller", func(t *testing.T) {
			root, request := handoffFixture(t)
			_, err := runHandoffAs(t, root, "bob", request, "bob")
			nothingSent(t, root, err, "the reviewer is you")
		}},
		{"the reviewer is the requester", func(t *testing.T) {
			root, request := handoffFixture(t)
			_, err := runHandoffAs(t, root, "bob", request, "alice")
			nothingSent(t, root, err, "the reviewer is the requester")
		}},
		{"a short or non-hex commit", func(t *testing.T) {
			root, request := handoffFixture(t)
			for _, commit := range []string{"0123456", strings.Repeat("g", 40), strings.ToUpper(handoffTestCommit)} {
				_, err := runHandoffAs(t, root, "bob", request, "carol", "--commit", commit)
				if GetExitCode(err) != 2 {
					t.Fatalf("--commit %s: err = %v, want a usage error", commit, err)
				}
			}
			nothingSent(t, root, nil, "")
		}},
		{"the same commit to a second reviewer sends both legs again", func(t *testing.T) {
			root, request := handoffFixture(t)
			if _, err := runHandoffAs(t, root, "bob", request, "carol"); err != nil {
				t.Fatal(err)
			}
			if _, err := runHandoffAs(t, root, "bob", request, "dave"); err != nil {
				t.Fatal(err)
			}
			if count(t, root, "carol") != 1 || count(t, root, "dave") != 1 || count(t, root, "alice") != 2 {
				t.Fatal("want one review each and two notices")
			}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, row.run)
	}
}
