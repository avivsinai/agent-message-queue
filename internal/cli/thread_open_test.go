package cli

import (
	"encoding/json"
	"math/rand/v2"
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

	type check func(t *testing.T, result threadOpenResult, names map[string]string)
	cases := []struct {
		name     string
		messages []threadOpenMessage
		args     []string
		want     []string // "<owner session>/<opener name>/<owner> <state> <owed_by_kind>"
		check    check
	}{
		{
			name: "review answered by a review_response is closed",
			messages: []threadOpenMessage{
				{name: "rr", from: "alpha@s1", to: "beta@s1", kind: format.KindReviewRequest},
				{name: "resp", from: "beta@s1", to: "alpha@s1", kind: format.KindReviewResponse, refs: []string{"rr"}},
			},
		},
		{
			name: "status from the owner acknowledges a question",
			messages: []threadOpenMessage{
				{name: "q", from: "alpha@s1", to: "beta@s1", kind: format.KindQuestion},
				{name: "st", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, refs: []string{"q"}},
			},
			want: []string{"s1/q/beta acknowledged agent"},
		},
		{
			name: "todo to user is owed by a human and completes with status done",
			messages: []threadOpenMessage{
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
			messages: []threadOpenMessage{
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
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "d", from: "beta@s1", to: "gamma@s1", kind: format.KindTodo, thread: "p2p/alpha__beta", refs: []string{"m"}},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/d/gamma pending agent"},
		},
		{
			// Design review of the thread open contract: the default thread
			// name repeats in every session, so a reference-free decision in
			// one session must not close work in another. The S2 decision
			// closes its one exact-participant candidate.
			name: "reference-free decision closes only its own session",
			messages: []threadOpenMessage{
				{name: "t1", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "t2", from: "alpha@s2", to: "beta@s2", kind: format.KindTodo},
				{name: "dec", from: "beta@s2", to: "alpha@s2", kind: format.KindDecision},
			},
			want: []string{"s1/t1/beta pending agent"},
		},
		{
			// Pro review finding 2 (trace T1): a second unreferenced todo in
			// the same thread was suppressed as a nudge of the first. Seen on a
			// copy of a live queue: unreferenced review requests folded into an
			// older one and vanished when it was answered.
			name: "two unreferenced todos are two obligations",
			messages: []threadOpenMessage{
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
			messages: []threadOpenMessage{
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
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "done", from: "beta@s1", to: "alpha@s1", kind: format.KindTodo, labels: []string{"done"}, refs: []string{"m"}},
			},
		},
		{
			// Pro review finding 3 (trace T4): owners were compared by
			// handle, so a cross-session send to the same handle opened
			// nothing and a same-named sender could close it.
			name: "same handle in another session is another participant",
			messages: []threadOpenMessage{
				{name: "m", from: "codex@s1", to: "codex@s2", kind: format.KindTodo, thread: "p2p/s1:codex__s2:codex", replyTo: "codex@s1"},
				{name: "dec", from: "codex@s3", to: "codex@s1", kind: format.KindDecision, thread: "p2p/s1:codex__s2:codex", replyTo: "codex@s3"},
			},
			want: []string{"s2/m/codex pending agent"},
		},
		{
			// Pro review finding 4 (trace T5): the answer route was built
			// from the obligation's session instead of reply_to.
			name: "reply destination follows reply_to",
			messages: []threadOpenMessage{
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
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "user@s1", kind: format.KindTodo},
				{name: "ans", from: "user@s2", to: "alpha@s1", kind: format.KindStatus, labels: []string{"done"}, refs: []string{"m"}, replyTo: "user@s2"},
			},
		},
		{
			// Pro review finding 8 (trace T7): a continuation whose parent
			// was cleaned up opened a reversed obligation.
			name: "missing direct target is unresolved lineage",
			messages: []threadOpenMessage{
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
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta", kind: format.KindTodo, replyTo: "alpha@s1", replyProject: "proj-a", noInbox: true},
			},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if want := []threadOpenCount{{Session: "s1", Count: 1}}; !slices.Equal(result.Coverage.OutboundRouted, want) {
					t.Fatalf("outbound_routed = %+v, want %+v", result.Coverage.OutboundRouted, want)
				}
			},
		},
		{
			// #1166 review: two clock-skewed children of one unreferenced
			// parent made a false cycle; the second was marked unresolved and
			// its decision never closed the todo.
			name: "two skewed children of one unreferenced parent both act",
			messages: []threadOpenMessage{
				{name: "d1", from: "alpha@s1", to: "beta@s1", kind: format.KindStatus, refs: []string{"st"}},
				{name: "d2", from: "alpha@s1", to: "beta@s1", kind: format.KindDecision, refs: []string{"st"}},
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "st", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus},
			},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if un := result.Coverage.UnresolvedLineage; len(un) != 0 {
					t.Fatalf("unresolved_lineage = %+v, want none", un)
				}
				if want := []threadOpenCount{{Session: "s1", Count: 2}}; !slices.Equal(result.Coverage.SkewDeferred, want) {
					t.Fatalf("skew_deferred = %+v, want %+v", result.Coverage.SkewDeferred, want)
				}
			},
		},
		{
			// #1166 review: an opener whose parent is created later must be
			// open at its own created time, so an unreferenced closer between
			// the two closes it.
			name: "opener with a later parent is open for an unreferenced closer",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"p"}},
				{name: "dec", from: "beta@s1", to: "alpha@s1", kind: format.KindDecision},
				{name: "p", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus},
			},
		},
		{
			// #1166 review: a todo closed by a reply is not a candidate, so an
			// unreferenced decision after the next todo has one candidate and
			// closes it.
			name: "unreferenced decision sees closures made by earlier replies",
			messages: []threadOpenMessage{
				{name: "m1", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "r", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, labels: []string{"done"}, refs: []string{"m1"}},
				{name: "m2", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "dec", from: "beta@s1", to: "alpha@s1", kind: format.KindDecision},
			},
		},
		{
			// #1166 review: a header whose created does not parse opened an
			// obligation (reported as expired under the default window).
			name: "invalid created header is skipped and opens nothing",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, badCreated: true},
			},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if len(result.Coverage.Skipped) != 1 || len(result.Coverage.Expired) != 0 {
					t.Fatalf("skipped = %+v, expired = %+v; want one skipped and nothing opened", result.Coverage.Skipped, result.Coverage.Expired)
				}
			},
		},
		{
			// #1166 algorithm review (C1): the closer table is exact; an
			// owner's withdrawn label does not close a question.
			name: "owner withdrawn acknowledges a question without closing it",
			messages: []threadOpenMessage{
				{name: "q", from: "alpha@s1", to: "beta@s1", kind: format.KindQuestion},
				{name: "w", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, labels: []string{"withdrawn"}, refs: []string{"q"}},
			},
			want: []string{"s1/q/beta acknowledged agent"},
		},
		{
			// #1166 algorithm review (C1): a requester's merged label does not
			// close a todo.
			name: "requester merged leaves a todo pending",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "mg", from: "alpha@s1", to: "beta@s1", kind: format.KindStatus, labels: []string{"merged"}, refs: []string{"m"}},
			},
			want: []string{"s1/m/beta pending agent"},
		},
		{
			// #1166 algorithm review (C1): a delegation only acknowledges the
			// old obligation, even with a closing label.
			name: "delegation with a closing label only acknowledges",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "d", from: "beta@s1", to: "gamma@s1", kind: format.KindTodo, labels: []string{"done"}, refs: []string{"m"}},
			},
			want: []string{"s1/m/beta acknowledged agent"},
		},
		{
			// #1166 algorithm review (C3): a reply to a message whose lineage
			// is empty is a settled no-op, never a wait or a cycle, even when
			// the reply is created first.
			name: "reply to an empty lineage is a no-op",
			messages: []threadOpenMessage{
				{name: "r", from: "beta@s1", to: "alpha@s1", kind: format.KindDecision, refs: []string{"st"}},
				{name: "st", from: "alpha@s1", to: "beta@s1", kind: format.KindStatus},
			},
			check: func(t *testing.T, result threadOpenResult, _ map[string]string) {
				if un := result.Coverage.UnresolvedLineage; len(un) != 0 {
					t.Fatalf("unresolved_lineage = %+v, want none", un)
				}
			},
		},
		{
			// #1166 algorithm review (C4): a same-kind reply continues only
			// when its sender and recipient both took part in the parent;
			// gamma did not, so alpha owes gamma.
			name: "same-kind reply from an outsider opens a new obligation",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "c", from: "gamma@s1", to: "alpha@s1", kind: format.KindTodo, thread: "p2p/alpha__beta", refs: []string{"m"}},
			},
			want: []string{"s1/m/beta pending agent", "s1/c/alpha pending agent"},
		},
		{
			// #1166 algorithm review (C5): an unreferenced decision created
			// before the todo and its question closes nothing.
			name: "decision before the opener closes nothing",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, at: 10 * time.Second},
				{name: "q", from: "beta@s1", to: "alpha@s1", kind: format.KindQuestion, refs: []string{"m"}, at: 11 * time.Second},
				{name: "d", from: "alpha@s1", to: "beta@s1", kind: format.KindDecision, at: 9500 * time.Millisecond},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/q/alpha pending agent"},
		},
		{
			// #1166 algorithm review (C5): moving the question before the
			// decision opens it first, so the decision closes it; the
			// question still acknowledges the todo once the todo opens.
			name: "question opened before the decision is closed by it",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, at: 10 * time.Second},
				{name: "q", from: "beta@s1", to: "alpha@s1", kind: format.KindQuestion, refs: []string{"m"}, at: 9 * time.Second},
				{name: "d", from: "alpha@s1", to: "beta@s1", kind: format.KindDecision, at: 9500 * time.Millisecond},
			},
			want: []string{"s1/m/beta acknowledged agent"},
		},
		{
			// #1166 code review: continuation pooled every participant of the
			// parent, so B delegating to C, a co-recipient of A's todo, was
			// taken for a continuation and C owed B nothing.
			name: "delegation to a co-recipient opens its own obligation",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1,gamma@s1", kind: format.KindTodo},
				{name: "r", from: "beta@s1", to: "gamma@s1", kind: format.KindTodo, thread: "p2p/alpha__beta", refs: []string{"m"}},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/m/gamma pending agent", "s1/r/gamma pending agent"},
		},
		{
			// #1166 code review: a reply to an opener with a missing parent
			// opened actionable work and was reported as a cycle.
			name: "reply below a missing parent opens nothing and names the root",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"gone"}},
				{name: "q", from: "beta@s1", to: "alpha@s1", kind: format.KindQuestion, refs: []string{"m"}},
			},
			check: func(t *testing.T, result threadOpenResult, names map[string]string) {
				got := []string{}
				for _, un := range result.Coverage.UnresolvedLineage {
					got = append(got, names[un.ID]+" "+names[un.MissingRef]+" "+un.Reason)
				}
				slices.Sort(got)
				if want := []string{"m gone missing", "q gone missing"}; !slices.Equal(got, want) {
					t.Fatalf("unresolved_lineage = %v, want %v", got, want)
				}
			},
		},
		{
			// #1166 code review: continuation was matched against the parent
			// header, so alpha, a recipient of r but not a party of r's own
			// obligation (gamma owes beta), could not ask beta for new work.
			name: "continuation follows the parent's lineage, not its header",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "r", from: "beta@s1", to: "alpha@s1,gamma@s1", kind: format.KindTodo, refs: []string{"m"}},
				{name: "s", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"r"}},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/r/gamma pending agent", "s1/s/beta pending agent"},
		},
		{
			// #1166 code review: a terminal delegation has an empty lineage,
			// so a same-kind reply to it is a new request, not a continuation.
			name: "reply to an empty-lineage delegation opens new work",
			messages: []threadOpenMessage{
				{name: "m", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo},
				{name: "r", from: "beta@s1", to: "gamma@s1", kind: format.KindTodo, labels: []string{"done"}, refs: []string{"m"}},
				{name: "s", from: "gamma@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"r"}},
			},
			want: []string{"s1/m/beta acknowledged agent", "s1/s/beta pending agent"},
		},
		{
			// #1166 code review: a same-kind reply whose parent is created
			// later cannot be classified at its own time; it was decided from
			// the parent header instead of its lineage.
			name: "same-kind reply to a later parent is ambiguous",
			messages: []threadOpenMessage{
				{name: "s", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"r"}, at: 10 * time.Second},
				{name: "r", from: "beta@s1", to: "alpha@s1", kind: format.KindTodo, labels: []string{"done"}, at: 20 * time.Second},
			},
			check: checkThreadOpenAmbiguous("s"),
		},
		{
			// #1166 code review: the same holds when the earlier parent itself
			// still waits for a later ancestor.
			name: "same-kind reply below an unsettled earlier parent is ambiguous",
			messages: []threadOpenMessage{
				{name: "p", from: "beta@s1", to: "alpha@s1", kind: format.KindTodo, labels: []string{"done"}, refs: []string{"l"}, at: 20 * time.Second},
				{name: "s", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, refs: []string{"p"}, at: 30 * time.Second},
				{name: "l", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, at: 40 * time.Second},
			},
			check: checkThreadOpenAmbiguous("s"),
		},
		{
			// Pro review finding 15: --limit must cut after reduction, or the
			// closed older obligation would take the only slot.
			name: "limit applies after reduction",
			messages: []threadOpenMessage{
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
			start := time.Now().UTC().Add(-time.Hour)
			ids, sessions := writeThreadOpenMessages(t, base, tc.messages, func(i int, m threadOpenMessage) time.Time {
				if m.age > 0 {
					return time.Now().UTC().Add(-m.age)
				}
				if m.at > 0 {
					return start.Add(m.at)
				}
				return start.Add(time.Duration(i+1) * time.Second)
			})
			result := runThreadOpenJSON(t, base, tc.args...)
			// Every session that holds a message is scanned, so a row that
			// expects work in one session to survive cannot pass by skipping
			// the other.
			if !slices.Equal(result.Sessions, sessions) {
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

// A reply acts on its parent's lineage once that lineage is settled, so
// moving a reply that opens nothing to any time before its parent changes
// nothing: the result equals the run where each reply directly follows its
// parent. File order changes nothing either. Only such effect-preserving
// moves are claimed; a clock change that moves an opening or a rule 5 match
// changes the result and has its own table rows. The set holds the #1166
// review cases: a skewed decision that visited its unreferenced parent
// early, two skewed children of one unreferenced parent, an opener whose
// parent is created later, and a reply closure an unreferenced decision
// must see.
func TestThreadOpenIgnoresFileOrderAndReplyClocks(t *testing.T) {
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe} {
		t.Setenv(key, "")
	}
	// Base order: every reply directly follows its parent.
	messages := []threadOpenMessage{
		{name: "m1", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, thread: "x"},
		{name: "st1", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, thread: "x"},
		{name: "dec1", from: "alpha@s1", to: "beta@s1", kind: format.KindDecision, thread: "x", refs: []string{"st1"}},

		{name: "m2", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, thread: "y"},
		{name: "st2", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, thread: "y"},
		{name: "d1", from: "alpha@s1", to: "beta@s1", kind: format.KindStatus, thread: "y", refs: []string{"st2"}},
		{name: "d2", from: "alpha@s1", to: "beta@s1", kind: format.KindDecision, thread: "y", refs: []string{"st2"}},

		{name: "rr1", from: "alpha@s1", to: "beta@s1", kind: format.KindReviewRequest, thread: "z"},
		{name: "resp", from: "beta@s1", to: "alpha@s1", kind: format.KindReviewResponse, thread: "z", refs: []string{"rr1"}},
		{name: "rr2", from: "alpha@s1", to: "beta@s1", kind: format.KindReviewRequest, thread: "z"},
		{name: "decz", from: "beta@s1", to: "alpha@s1", kind: format.KindDecision, thread: "z"},

		{name: "t", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, thread: "w"},
		{name: "dg", from: "beta@s1", to: "gamma@s1", kind: format.KindTodo, thread: "w", refs: []string{"t"}},

		{name: "mp", from: "alpha@s1", to: "beta@s1", kind: format.KindTodo, thread: "v", refs: []string{"pp"}},
		{name: "decp", from: "beta@s1", to: "alpha@s1", kind: format.KindDecision, thread: "v"},
		{name: "pp", from: "beta@s1", to: "alpha@s1", kind: format.KindStatus, thread: "v"},
	}
	moved := map[string]string{"dec1": "st1", "d1": "st2", "d2": "st2", "resp": "rr1"} // reply -> parent
	want := "s1/dg/gamma pending; s1/t/beta acknowledged | ambiguous:  | unresolved: "

	for seed := uint64(0); seed < 4; seed++ {
		base := filepath.Join(t.TempDir(), defaultCoopRoot)
		start := time.Now().UTC().Add(-time.Hour)
		times := map[string]time.Time{}
		for i, m := range messages {
			times[m.name] = start.Add(time.Duration(i+1) * 10 * time.Second)
		}
		if seed > 0 {
			rng := rand.New(rand.NewPCG(seed, seed))
			for reply, parent := range moved {
				times[reply] = times[parent].Add(-time.Duration(1+rng.IntN(300)) * time.Second)
			}
		}
		ids, _ := writeThreadOpenMessages(t, base, messages, func(_ int, m threadOpenMessage) time.Time { return times[m.name] })
		result := runThreadOpenJSON(t, base)
		names := map[string]string{}
		for name, id := range ids {
			names[id] = name
		}
		obligations, ambiguous, unresolved := []string{}, []string{}, []string{}
		for _, ob := range result.Obligations {
			obligations = append(obligations, ob.OwedBy.Session+"/"+names[ob.Opener.ID]+"/"+ob.OwedBy.Handle+" "+ob.State)
		}
		for _, amb := range result.Coverage.Ambiguous {
			ambiguous = append(ambiguous, names[amb.ID])
		}
		for _, un := range result.Coverage.UnresolvedLineage {
			unresolved = append(unresolved, names[un.ID])
		}
		slices.Sort(obligations)
		slices.Sort(ambiguous)
		slices.Sort(unresolved)
		got := strings.Join(obligations, "; ") + " | ambiguous: " + strings.Join(ambiguous, ", ") + " | unresolved: " + strings.Join(unresolved, ", ")
		if got != want {
			t.Fatalf("seed %d:\n got %s\nwant %s", seed, got, want)
		}
	}
}

// checkThreadOpenAmbiguous asserts that exactly the named messages are
// reported ambiguous because their parent's lineage was not settled yet.
func checkThreadOpenAmbiguous(want ...string) func(*testing.T, threadOpenResult, map[string]string) {
	return func(t *testing.T, result threadOpenResult, names map[string]string) {
		t.Helper()
		got := []string{}
		for _, amb := range result.Coverage.Ambiguous {
			if amb.Reason == threadOpenUnsettledParent {
				got = append(got, names[amb.ID])
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("ambiguous with an unsettled parent = %v, want %v (all: %+v)", got, want, result.Coverage.Ambiguous)
		}
	}
}

// threadOpenMessage is one message of a thread open fixture. from is
// handle@session and to is a comma-separated list of handle@session. The sender's outbox copy lands in its session and the
// recipient's inbox copy in the recipient's session, unless noInbox marks a
// send routed outside the scan.
type threadOpenMessage struct {
	name, from, to, kind, thread string
	refs, labels                 []string // refs name other messages; an unknown name is a missing id
	replyTo, replyProject        string
	noInbox, badCreated          bool
	age                          time.Duration // created this long before now
	at                           time.Duration // created this long after the table's start
}

// writeThreadOpenMessages writes messages under base with the created time
// that created returns for each, and returns their ids by name and the
// sorted sessions that hold a copy.
func writeThreadOpenMessages(t *testing.T, base string, messages []threadOpenMessage, created func(int, threadOpenMessage) time.Time) (map[string]string, []string) {
	t.Helper()
	times := make([]time.Time, len(messages))
	ids := map[string]string{}
	newID := func(at time.Time) string {
		id, err := format.NewMessageID(at)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, m := range messages {
		times[i] = created(i, m)
		ids[m.name] = newID(times[i])
	}
	sessions := []string{}
	for i, m := range messages {
		from, fromSession, _ := strings.Cut(m.from, "@")
		var to []string
		for _, recipient := range strings.Split(m.to, ",") {
			handle, _, _ := strings.Cut(recipient, "@")
			to = append(to, handle)
		}
		refs := []string{}
		for _, ref := range m.refs {
			if _, ok := ids[ref]; !ok {
				ids[ref] = newID(times[i].Add(-time.Minute))
			}
			refs = append(refs, ids[ref])
		}
		threadID := m.thread
		if threadID == "" {
			threadID = canonicalP2P(from, to[0])
		}
		id := ids[m.name]
		createdField := times[i].Format(time.RFC3339Nano)
		if m.badCreated {
			createdField = "yesterday"
		}
		data, err := format.Message{Header: format.Header{
			Schema: format.CurrentSchema, ID: id, From: from, To: to,
			Thread: threadID, Created: createdField,
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
		for _, recipient := range strings.Split(m.to, ",") {
			if m.noInbox {
				break
			}
			handle, toSession, _ := strings.Cut(recipient, "@")
			toRoot := filepath.Join(base, toSession)
			if err := fsq.EnsureAgentDirs(toRoot, handle); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(toRoot, handle), id+".md"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			sessions = append(sessions, toSession)
		}
	}
	slices.Sort(sessions)
	return ids, slices.Compact(sessions)
}

func runThreadOpenJSON(t *testing.T, base string, extra ...string) threadOpenResult {
	t.Helper()
	args := append([]string{"open", "--json", "--all-sessions", "--root", filepath.Join(base, "s1")}, extra...)
	stdout, stderr := captureOutput(t, func() error { return runThread(args) })
	var result threadOpenResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	return result
}
