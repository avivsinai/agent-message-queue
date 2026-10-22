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

	// from and to are handle@session. The sender's outbox copy lands in its
	// session and the recipient's inbox copy in the recipient's session,
	// unless noInbox marks a send routed outside the scan.
	type message struct {
		name, from, to, kind, thread string
		refs, labels                 []string // refs name earlier messages; an unknown name is a missing id
		replyTo, replyProject        string
		noInbox                      bool
		age                          time.Duration // default: ordered within the last hour
	}
	type check func(t *testing.T, result threadOpenResult, names map[string]string)
	cases := []struct {
		name     string
		messages []message
		args     []string
		want     []string // "<owner session>/<opener name>/<owner> <state> <owed_by_kind>"
		check    check
	}{
		{
			name: "review answered by a review_response is closed",
			messages: []message{
				{name: "rr", from: "alpha@s1", to: "beta@s1", kind: format.KindReviewRequest},
				{name: "resp", from: "beta@s1", to: "alpha@s1", kind: format.KindReviewResponse, refs: []string{"rr"}},
			},
		},
		{
			name: "status from the owner acknowledges a question",
			messages: []message{
				{name: "q", from: "alpha@s1", to: "beta@s1", kind: format.KindQuestion},
				{name: "st", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, refs: []string{"q"}},
			},
			want: []string{"s1/q/beta acknowledged agent"},
		},
		{
			name: "todo to user is owed by a human and completes with status done",
			messages: []message{
				{name: "t", from: "alpha@s1", to: "user@s1", kind: format.KindTodo},
			},
			want: []string{"s1/t/user pending human"},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if got := result.Obligations[0].Completion; got.Kind != format.KindStatus || !slices.Equal(got.Labels, []string{"done"}) {
					t.Fatalf("completion = %+v, want status [done]", got)
				}
			},
		},
		{
			name: "file older than max-age is counted unread",
			messages: []message{
				{name: "old", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, age: 20 * 24 * time.Hour},
			},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if want := []threadOpenCount{{Session: "s1", Count: 2}}; !slices.Equal(result.Coverage.PrunedFiles, want) {
					t.Fatalf("pruned_files = %+v, want %+v", result.Coverage.PrunedFiles, want)
				}
			},
		},
		{
			// Design review of the thread open contract: a todo delegated
			// with refs to its opener opens the new recipient's obligation
			// instead of folding into the first.
			name: "delegated todo opens the new recipient's obligation",
			messages: []message{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "d", from: "beta@s1", to: "gamma@s1", kind: format.KindTodo, thread: "p2p/alpha__beta", refs: []string{"m"}},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/d/gamma pending agent"},
		},
		{
			// Design review of the thread open contract: the default thread
			// name repeats in every session, so a reference-free decision in
			// one session must not close work in another.
			name: "reference-free decision closes only its own session",
			messages: []message{
				{name: "t1", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "t2", from: "alpha@s2", to: "beta@s2", kind: format.KindTodo},
				{name: "dec", from: "beta@s2", to: "alpha@s2", kind: format.KindDecision},
			},
			want: []string{"s1/t1/beta pending agent"},
		},
		{
			// Pro review finding 2 (trace T1): a second unreferenced todo in
			// the same thread was suppressed as a nudge of the first.
			name: "two unreferenced todos are two obligations",
			messages: []message{
				{name: "m1", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "m2", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
			},
			want: []string{"s1/m1/beta pending agent", "s1/m2/beta pending agent"},
		},
		{
			// Pro review finding 2 (trace T2): a decision that answers the
			// owner's question inherited the todo's ref and closed the
			// unfinished todo.
			name: "decision closes only its direct target",
			messages: []message{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "q", from: "beta@s1", to: "alpha@s1", kind: format.KindQuestion, refs: []string{"m"}},
				{name: "d", from: "alpha@s1", to: "beta@s1", kind: format.KindDecision, refs: []string{"m", "q"}},
			},
			want: []string{"s1/m/beta acknowledged agent"},
		},
		{
			// Pro review finding 2 (trace T3): `amq reply --labels done` to a
			// todo keeps kind todo and opened a new obligation owed by the
			// requester.
			name: "terminal same-kind reply closes and opens nothing",
			messages: []message{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "done", from: "beta@s1", to: "alpha@s1", kind: format.KindTodo, labels: []string{"done"}, refs: []string{"m"}},
			},
		},
		{
			// Pro review finding 3 (trace T4): owners were compared by
			// handle, so a cross-session send to the same handle opened
			// nothing and a same-named sender could close it.
			name: "same handle in another session is another participant",
			messages: []message{
				{name: "m", from: "codex@s1", to: "codex@s2", kind: format.KindTodo, thread: "p2p/s1:codex__s2:codex", replyTo: "codex@s1"},
				{name: "dec", from: "codex@s3", to: "codex@s1", kind: format.KindDecision, thread: "p2p/s1:codex__s2:codex", replyTo: "codex@s3"},
			},
			want: []string{"s2/m/codex pending agent"},
		},
		{
			// Pro review finding 4 (trace T5): the answer route was built
			// from the obligation's session instead of reply_to.
			name: "reply destination follows reply_to",
			messages: []message{
				{name: "q", from: "alpha@s1", to: "user@s2", kind: format.KindQuestion, thread: "p2p/s1:alpha__s2:user", replyTo: "alpha@s1"},
			},
			want: []string{"s2/q/user pending human"},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				want := threadOpenDestination{Handle: "alpha", Session: "s1", Source: "reply_to"}
				if got := result.Obligations[0].ReplyDestination; got == nil || *got != want {
					t.Fatalf("reply_destination = %+v, want %+v", got, want)
				}
			},
		},
		{
			// Trace T6: the operator answers from a shell pinned to another
			// session of the same project.
			name: "reserved user closes its todo from another session by reference",
			messages: []message{
				{name: "m", from: "alpha@s1", to: "user@s1", kind: format.KindTodo},
				{name: "ans", from: "user@s2", to: "alpha@s1", kind: format.KindStatus, labels: []string{"done"}, refs: []string{"m"}, replyTo: "user@s2"},
			},
		},
		{
			// Pro review finding 8 (trace T7): a continuation whose parent
			// was cleaned up opened a reversed obligation.
			name: "missing direct target is unresolved lineage",
			messages: []message{
				{name: "c", from: "beta@s1", to: "alpha@s1", kind: format.KindTodo, refs: []string{"gone"}},
			},
			check: func(t *testing.T, result threadOpenResult, names map[string]string) {
				un := result.Coverage.UnresolvedLineage
				if len(un) != 1 || names[un[0].ID] != "c" || names[un[0].MissingRef] != "gone" {
					t.Fatalf("unresolved_lineage = %+v, want c missing gone", un)
				}
			},
		},
		{
			// Trace T8: a send routed to another project leaves only the
			// outbox copy, which carries no target metadata.
			name: "routed send without an inbox copy is counted, not owned",
			messages: []message{
				{name: "m", from: "alpha@s1", to: "beta", kind: format.KindTodo, replyTo: "alpha@s1", replyProject: "proj-a", noInbox: true},
			},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if want := []threadOpenCount{{Session: "s1", Count: 1}}; !slices.Equal(result.Coverage.OutboundRouted, want) {
					t.Fatalf("outbound_routed = %+v, want %+v", result.Coverage.OutboundRouted, want)
				}
			},
		},
		{
			// Pro review finding 15: --limit must cut after reduction, or the
			// closed older obligation would take the only slot.
			name: "limit applies after reduction",
			messages: []message{
				{name: "a", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "b", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "fin", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, labels: []string{"done"}, refs: []string{"a"}},
			},
			args: []string{"--limit", "1"},
			want: []string{"s1/b/beta pending agent"},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if want := (threadOpenTruncated{Limit: 1}); result.Coverage.TruncatedByLimit != want {
					t.Fatalf("truncated_by_limit = %+v, want %+v", result.Coverage.TruncatedByLimit, want)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), defaultCoopRoot)
			ids := map[string]string{}
			idFor := func(name string, created time.Time) string {
				if _, ok := ids[name]; !ok {
					id, err := format.NewMessageID(created)
					if err != nil {
						t.Fatal(err)
					}
					ids[name] = id
				}
				return ids[name]
			}
			start := time.Now().UTC().Add(-time.Hour)
			sessions := []string{}
			for i, m := range tc.messages {
				from, fromSession, _ := strings.Cut(m.from, "@")
				to, toSession, _ := strings.Cut(m.to, "@")
				created := start.Add(time.Duration(i+1) * time.Second)
				if m.age > 0 {
					created = time.Now().UTC().Add(-m.age)
				}
				id := idFor(m.name, created)
				refs := []string{}
				for _, ref := range m.refs {
					refs = append(refs, idFor(ref, start))
				}
				threadID := m.thread
				if threadID == "" {
					threadID = canonicalP2P(from, to)
				}
				data, err := format.Message{Header: format.Header{
					Schema: format.CurrentSchema, ID: id, From: from, To: []string{to},
					Thread: threadID, Created: created.Format(time.RFC3339Nano),
					Refs: refs, Kind: m.kind, Labels: m.labels, Priority: format.PriorityNormal,
					ReplyTo: m.replyTo, ReplyProject: m.replyProject, FromProject: m.replyProject,
				}, Body: "body\n"}.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				fromRoot := filepath.Join(base, fromSession)
				if err := fsq.EnsureAgentDirs(fromRoot, from); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fsq.AgentOutboxSent(fromRoot, from), id+".md"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				sessions = append(sessions, fromSession)
				if !m.noInbox {
					toRoot := filepath.Join(base, toSession)
					if err := fsq.EnsureAgentDirs(toRoot, to); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(toRoot, to), id+".md"), data, 0o600); err != nil {
						t.Fatal(err)
					}
					sessions = append(sessions, toSession)
				}
			}

			args := append([]string{"open", "--json", "--all-sessions", "--root", filepath.Join(base, "s1")}, tc.args...)
			stdout, stderr := captureOutput(t, func() error { return runThread(args) })
			var result threadOpenResult
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("unmarshal: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
			}
			// Every session that holds a message is scanned, so a row that
			// expects work in one session to survive cannot pass by skipping
			// the other.
			slices.Sort(sessions)
			if sessions = slices.Compact(sessions); !slices.Equal(result.Sessions, sessions) {
				t.Fatalf("sessions = %v, want %v", result.Sessions, sessions)
			}
			names := map[string]string{}
			for name, id := range ids {
				names[id] = name
			}
			got := []string{}
			for _, ob := range result.Obligations {
				got = append(got, ob.OwedBy.Session+"/"+names[ob.Opener.ID]+"/"+ob.OwedBy.Handle+" "+ob.State+" "+ob.OwedByKind)
			}
			want := append([]string{}, tc.want...)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("obligations:\n got %s\nwant %s", strings.Join(got, "; "), strings.Join(want, "; "))
			}
			if tc.check != nil {
				tc.check(t, result, names)
			}
		})
	}
}
