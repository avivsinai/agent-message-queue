package core_test

import (
	"errors"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// claudeApproval is the fake runtime presented as a Claude session whose
// questions are tool approvals, as the Claude attachment projects them.
type claudeApproval struct {
	*fake.Runtime
	approve, reject string
}

func (c claudeApproval) Inspect() protocol.Session {
	s := c.Runtime.Inspect()
	s.Harness = "claude_code"
	return s
}

func (c claudeApproval) Subscribe(fn func(core.NativeEvent)) func() {
	return c.Runtime.Subscribe(func(ev core.NativeEvent) {
		if ev.Type == core.EventQuestion && ev.Interaction != nil {
			in := *ev.Interaction
			in.Kind, in.ApproveOption, in.RejectOption = "approval", c.approve, c.reject
			ev.Interaction = &in
		}
		fn(ev)
	})
}

// Bead agent-message-queue-611.42.2: the Buzz Desktop agent relays the
// owner's ❌ over the local socket. A local client may deny the Claude
// approval of a request it submitted, and nothing else: allow, an approval
// with no reject option, and a stale interaction stay refused.
func TestLocalSocketMayOnlyDenyItsClaudeApproval(t *testing.T) {
	local := core.Source{Host: core.LocalHost}
	cases := []struct {
		name            string
		approve, reject string
		interaction     string
		option          string
		want            protocol.Code // "" accepted
	}{
		{name: "deny", reject: "deny", interaction: "i_1", option: "deny"},
		{name: "allow", approve: "allow", reject: "deny", interaction: "i_1", option: "allow", want: protocol.CodeUnshared},
		{name: "no reject option", interaction: "i_1", option: "deny", want: protocol.CodeUnshared},
		{name: "stale interaction", reject: "deny", interaction: "i_0", option: "deny", want: protocol.CodeAlreadyResolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, now := openStore(t)
			rt := fake.New("fake", "e_1")
			ep := core.New(core.Config{Store: store, Now: now})
			ep.Register(claudeApproval{Runtime: rt, approve: tc.approve, reject: tc.reject})
			id := "11111111-1111-4111-8111-1111111142f2"
			if _, err := ep.Handle(submitCmd(id), local); err != nil {
				t.Fatal(err)
			}
			rt.Question(id, "i_1", []string{"allow", "deny"})
			_, err := ep.Handle(&protocol.Command{
				Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: protocol.EncodeRef(core.LocalHost, "fake", id),
				TargetID: "fake", Epoch: "e_1", InteractionID: tc.interaction, Option: tc.option,
			}, local)
			answers := rt.Snapshot().Answers
			if tc.want == "" {
				if err != nil || len(answers) != 1 || answers[0].Option != "deny" {
					t.Fatalf("local deny = %v, answers %v; want the deny delivered", err, answers)
				}
				return
			}
			var ref *protocol.Refusal
			if !errors.As(err, &ref) || ref.Code != tc.want || len(answers) != 0 {
				t.Fatalf("answer = %v, answers %v; want refused %s and nothing delivered", err, answers, tc.want)
			}
		})
	}
}
