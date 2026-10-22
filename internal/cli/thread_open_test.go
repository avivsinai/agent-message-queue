package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestThreadOpenListsOpenObligations(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}

	type message struct {
		name, session, from, to, kind, thread string
		refs, labels                          []string
		age                                   time.Duration // default: ordered within the last hour
	}
	cases := []struct {
		name     string
		messages []message
		want     []string // "<session>/<opener name>/<owner> <state> <owed_by_kind>"
		pruned   []threadOpenPruned
	}{
		{
			name: "review answered by a review_response is closed",
			messages: []message{
				{name: "rr", session: "s1", from: "alpha", to: "beta", kind: format.KindReviewRequest},
				{name: "resp", session: "s1", from: "beta", to: "alpha", kind: format.KindReviewResponse, refs: []string{"rr"}},
			},
		},
		{
			name: "same-kind continuation from the owner acknowledges a question",
			messages: []message{
				{name: "q", session: "s1", from: "alpha", to: "beta", kind: format.KindQuestion},
				{name: "q2", session: "s1", from: "beta", to: "alpha", kind: format.KindQuestion, refs: []string{"q"}},
			},
			want: []string{"s1/q/beta acknowledged agent"},
		},
		{
			name: "status without done acknowledges a todo",
			messages: []message{
				{name: "t", session: "s1", from: "alpha", to: "beta", kind: format.KindTodo},
				{name: "st", session: "s1", from: "beta", to: "alpha", kind: format.KindStatus, refs: []string{"t"}},
			},
			want: []string{"s1/t/beta acknowledged agent"},
		},
		{
			name: "todo to user is owed by a human",
			messages: []message{
				{name: "t", session: "s1", from: "alpha", to: "user", kind: format.KindTodo},
			},
			want: []string{"s1/t/user pending human"},
		},
		{
			name: "file older than max-age is counted unread",
			messages: []message{
				{name: "old", session: "s1", from: "alpha", to: "beta", kind: format.KindTodo, age: 20 * 24 * time.Hour},
			},
			pruned: []threadOpenPruned{{Session: "s1", Count: 2}},
		},
		{
			// Pre-merge review finding on the thread open contract: a todo
			// delegated with refs to its opener must open the new
			// recipient's obligation instead of folding into the first.
			name: "delegated todo opens the new recipient's obligation",
			messages: []message{
				{name: "m", session: "s1", from: "alpha", to: "beta", kind: format.KindTodo},
				{name: "d", session: "s1", from: "beta", to: "gamma", kind: format.KindTodo, thread: "p2p/alpha__beta", refs: []string{"m"}},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/d/gamma pending agent"},
		},
		{
			// Pre-merge review finding on the thread open contract: the
			// default thread name repeats in every session, so a
			// reference-free decision in one session must not close work in
			// another.
			name: "reference-free decision closes only its own session",
			messages: []message{
				{name: "t1", session: "s1", from: "alpha", to: "beta", kind: format.KindTodo},
				{name: "t2", session: "s2", from: "alpha", to: "beta", kind: format.KindTodo},
				{name: "dec", session: "s2", from: "beta", to: "alpha", kind: format.KindDecision},
			},
			want: []string{"s1/t1/beta pending agent"},
		},
		{
			// Observed on a copy of a live queue: with --all-sessions, a
			// second review request sent without refs into the same thread
			// was folded into the first and vanished when the first was
			// answered.
			name: "reference-free request in a busy thread keeps its own obligation",
			messages: []message{
				{name: "rr1", session: "s1", from: "alpha", to: "beta", kind: format.KindReviewRequest},
				{name: "rr2", session: "s1", from: "alpha", to: "beta", kind: format.KindReviewRequest},
				{name: "resp", session: "s1", from: "beta", to: "alpha", kind: format.KindReviewResponse, refs: []string{"rr1"}},
			},
			want: []string{"s1/rr2/beta pending agent"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), defaultCoopRoot)
			ids := map[string]string{}
			start := time.Now().UTC().Add(-time.Hour)
			for i, m := range tc.messages {
				root := filepath.Join(base, m.session)
				for _, agent := range []string{m.from, m.to} {
					if err := fsq.EnsureAgentDirs(root, agent); err != nil {
						t.Fatal(err)
					}
				}
				created := start.Add(time.Duration(i) * time.Second)
				if m.age > 0 {
					created = time.Now().UTC().Add(-m.age)
				}
				id, err := format.NewMessageID(created)
				if err != nil {
					t.Fatal(err)
				}
				ids[m.name] = id
				refs := []string{}
				for _, ref := range m.refs {
					refs = append(refs, ids[ref])
				}
				threadID := m.thread
				if threadID == "" {
					threadID = canonicalP2P(m.from, m.to)
				}
				data, err := format.Message{Header: format.Header{
					Schema: format.CurrentSchema, ID: id, From: m.from, To: []string{m.to},
					Thread: threadID, Created: created.Format(time.RFC3339Nano),
					Refs: refs, Kind: m.kind, Labels: m.labels, Priority: format.PriorityNormal,
				}, Body: "body\n"}.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := deliverToInboxForTest(t, root, m.to, id+".md", data); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fsq.AgentOutboxSent(root, m.from), id+".md"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			stdout, stderr := captureOutput(t, func() error {
				return runThread([]string{"open", "--json", "--all-sessions", "--root", filepath.Join(base, "s1")})
			})
			var result threadOpenResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("unmarshal: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
			}
			names := map[string]string{}
			for name, id := range ids {
				names[id] = name
			}
			got := []string{}
			for _, ob := range result.Obligations {
				got = append(got, ob.Session+"/"+names[ob.Opener.ID]+"/"+ob.OwedBy+" "+ob.State+" "+ob.OwedByKind)
			}
			want := append([]string{}, tc.want...)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("obligations:\n got %s\nwant %s", strings.Join(got, "; "), strings.Join(want, "; "))
			}
			// Every session that holds a message is scanned, so a row that
			// expects work in one session to survive cannot pass by skipping
			// the other.
			wantSessions := []string{}
			for _, m := range tc.messages {
				if !slices.Contains(wantSessions, m.session) {
					wantSessions = append(wantSessions, m.session)
				}
			}
			slices.Sort(wantSessions)
			if !slices.Equal(result.Sessions, wantSessions) {
				t.Fatalf("sessions = %v, want %v", result.Sessions, wantSessions)
			}
			wantPruned := tc.pruned
			if wantPruned == nil {
				wantPruned = []threadOpenPruned{}
			}
			if !slices.Equal(result.PrunedFiles, wantPruned) {
				t.Fatalf("pruned_files = %+v, want %+v", result.PrunedFiles, wantPruned)
			}
		})
	}
}
