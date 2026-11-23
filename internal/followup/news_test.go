package followup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// sendRequest writes alice's todo to bob, sent at sent, into alice's
// outbox/sent as a send does, and returns its header.
func sendRequest(t *testing.T, root *fsq.DeliveryRoot, sent time.Time, replyTo string) format.Header {
	t.Helper()
	id, err := format.NewMessageID(sent)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: 1, ID: id, From: "alice", To: []string{"bob"}, Thread: "p2p/alice__bob", Subject: "Review the parser",
		Created: sent.Format(time.RFC3339Nano), Kind: format.KindTodo, ReplyTo: replyTo,
	}, Body: "please"}
	if _, err := root.WriteFileAtomic(fsq.MailboxRootRelativePath("alice", fsq.MailboxOutboxSent), id+".md", marshal(t, msg), 0o600); err != nil {
		t.Fatal(err)
	}
	return msg.Header
}

// armNews arms the news timer of request; a routed send records the session
// it delivered to.
func armNews(t *testing.T, root *fsq.DeliveryRoot, request format.Header, sent time.Time, after time.Duration, targetSession string) Timer {
	t.Helper()
	timer, err := NewNews(request.ID, "bob", request.Thread, request.Subject, request.Kind, sent, after)
	if err != nil {
		t.Fatal(err)
	}
	timer.TargetSession = targetSession
	if err := Write(root, "alice", timer); err != nil {
		t.Fatal(err)
	}
	return timer
}

// deliverToAlice puts a message from `from` that refers to the request into
// alice's inbox, one minute after the request.
func deliverToAlice(t *testing.T, root *fsq.DeliveryRoot, request format.Header, from, kind string, labels []string, replyTo string) {
	t.Helper()
	at := mustCreated(t, request).Add(time.Minute)
	id, err := format.NewMessageID(at)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: 1, ID: id, From: from, To: []string{"alice"}, Thread: request.Thread,
		Created: at.Format(time.RFC3339Nano), Refs: []string{request.ID}, Kind: kind, Labels: labels, ReplyTo: replyTo,
	}, Body: "ok"}
	if _, err := fsq.DeliverToInbox(root, "alice", id+".md", marshal(t, msg)); err != nil {
		t.Fatal(err)
	}
}

func mustCreated(t *testing.T, h format.Header) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, h.Created)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func noticeLabels(t *testing.T, base, noticeID string) []string {
	t.Helper()
	h, err := format.ReadHeaderFile(filepath.Join(fsq.AgentInboxNew(base, "alice"), noticeID+".md"))
	if err != nil {
		t.Fatalf("notice %s: %v", noticeID, err)
	}
	return h.Labels
}

// Observed: a worker read three orders and sent nothing for 8 hours; only the
// completion follow-up could report it.
func TestPromoteNewsTimerNoticesASilentRecipientOnce(t *testing.T) {
	base, root := openTestRoot(t)
	sent := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	request := sendRequest(t, root, sent, "")
	timer := armNews(t, root, request, sent, 20*time.Minute, "")

	if err := Promote(root, "alice", sent.Add(21*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if names := inboxNames(t, base); !slices.Equal(names, []string{timer.NoticeID + ".md"}) {
		t.Fatalf("inbox = %v, want the one news notice", names)
	}
	msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(base, "alice"), timer.NoticeID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(msg.Header.Labels, []string{Label, NewsLabel}) || !slices.Equal(msg.Header.Refs, []string{request.ID}) {
		t.Fatalf("notice header = %+v", msg.Header)
	}
	if want := "No reply seen from bob about \"Review the parser\" since it was sent 20m ago.\n\namq trace " + request.ID + " --follow\n"; msg.Body != want {
		t.Fatalf("notice body = %q, want %q", msg.Body, want)
	}
	if timers, err := List(root, "alice"); err != nil || len(timers) != 0 {
		t.Fatalf("timers after the notice = %v (err %v), want none", timers, err)
	}
	if err := Promote(root, "alice", sent.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if names := inboxNames(t, base); len(names) != 1 {
		t.Fatalf("a second pass left %v, want the one notice", names)
	}
}

func TestPromoteNewsAndCompletionTimers(t *testing.T) {
	sent := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	type step struct {
		at   time.Duration
		want []string // labels of the notices in the inbox after this pass, in due order
		left int      // timers left
	}
	for _, tc := range []struct {
		name       string
		news       time.Duration
		completion time.Duration
		legacy     bool
		arrange    func(t *testing.T, root *fsq.DeliveryRoot, request format.Header)
		replyTo    string
		// targetSession is the session a routed send delivered to.
		targetSession string
		steps         []step
	}{
		{name: "progress from the recipient ends the news clock", news: 20 * time.Minute,
			arrange: func(t *testing.T, root *fsq.DeliveryRoot, r format.Header) {
				deliverToAlice(t, root, r, "bob", format.KindStatus, nil, "")
			}, steps: []step{{21 * time.Minute, nil, 0}}},
		{name: "a question from the recipient ends the news clock", news: 20 * time.Minute,
			arrange: func(t *testing.T, root *fsq.DeliveryRoot, r format.Header) {
				deliverToAlice(t, root, r, "bob", format.KindQuestion, nil, "")
			}, steps: []step{{21 * time.Minute, nil, 0}}},
		{name: "the closing reply ends the news clock", news: 20 * time.Minute,
			arrange: func(t *testing.T, root *fsq.DeliveryRoot, r format.Header) {
				deliverToAlice(t, root, r, "bob", format.KindStatus, []string{"done"}, "")
			}, steps: []step{{21 * time.Minute, nil, 0}}},
		{name: "a reply from another handle does not end it", news: 20 * time.Minute,
			arrange: func(t *testing.T, root *fsq.DeliveryRoot, r format.Header) {
				deliverToAlice(t, root, r, "carol", format.KindStatus, nil, "")
			}, steps: []step{{21 * time.Minute, []string{NewsLabel}, 0}}},
		{name: "a routed request settles by a reply from its recorded session", news: 20 * time.Minute, replyTo: "alice@lead", targetSession: "build",
			arrange: func(t *testing.T, root *fsq.DeliveryRoot, r format.Header) {
				deliverToAlice(t, root, r, "bob", format.KindStatus, nil, "bob@build")
			}, steps: []step{{21 * time.Minute, nil, 0}}},
		{name: "news at 10m comes first and the completion timer stays", news: 10 * time.Minute, completion: time.Hour,
			steps: []step{{10 * time.Minute, []string{NewsLabel}, 1}}},
		{name: "completion at 5m and news at 10m both come once", news: 10 * time.Minute, completion: 5 * time.Minute,
			steps: []step{
				{5 * time.Minute, []string{Label}, 1},
				{10 * time.Minute, []string{Label, NewsLabel}, 0},
				{time.Hour, []string{Label, NewsLabel}, 0},
			}},
		{name: "a timer with no kind is a completion timer", completion: 20 * time.Minute, legacy: true,
			arrange: func(t *testing.T, root *fsq.DeliveryRoot, r format.Header) {
				deliverToAlice(t, root, r, "bob", format.KindStatus, nil, "")
			}, steps: []step{{21 * time.Minute, []string{Label}, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, root := openTestRoot(t)
			request := sendRequest(t, root, sent, tc.replyTo)
			var notices []Timer
			if tc.completion > 0 {
				timer, err := New(request.ID, "bob", request.Thread, request.Subject, request.Kind, sent, tc.completion)
				if err != nil {
					t.Fatal(err)
				}
				if tc.legacy {
					// The record an earlier release wrote: no kind field.
					raw := `{"schema":1,"request_id":"` + request.ID + `","to":"bob","thread":"p2p/alice__bob","request_kind":"todo","sent":"` +
						timer.Sent.Format(time.RFC3339Nano) + `","due":"` + timer.Due.Format(time.RFC3339Nano) + `","notice_id":"` + timer.NoticeID + `"}` + "\n"
					if err := root.EnsureAgentDir("alice", fsq.MailboxFollowUps); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(fsq.AgentFollowUps(base, "alice"), request.ID+".json"), []byte(raw), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := Write(root, "alice", timer); err != nil {
					t.Fatal(err)
				}
				notices = append(notices, timer)
			}
			if tc.news > 0 {
				notices = append(notices, armNews(t, root, request, sent, tc.news, tc.targetSession))
			}
			if tc.arrange != nil {
				tc.arrange(t, root, request)
			}
			for _, s := range tc.steps {
				if err := Promote(root, "alice", sent.Add(s.at)); err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, n := range notices {
					if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(base, "alice"), n.NoticeID+".md")); err == nil {
						labels := noticeLabels(t, base, n.NoticeID)
						got = append(got, labels[len(labels)-1])
					}
				}
				if !slices.Equal(got, s.want) {
					t.Fatalf("at %s the notices are %v, want %v", s.at, got, s.want)
				}
				if timers, err := List(root, "alice"); err != nil || len(timers) != s.left {
					t.Fatalf("at %s the timers are %v (err %v), want %d", s.at, timers, err, s.left)
				}
			}
		})
	}
}
