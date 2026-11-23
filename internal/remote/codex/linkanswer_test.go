package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// linkChainSource publishes the attachment's dialogs to the link and
// records the link's answers: the real render — dialog.RenderLinkInteraction,
// the one render publication and the write share — the real directory, and
// the real pinned write primitive. The production machine source and its
// policy live in cmd/amq-remote; this one carries a fixture policy so the
// chain from a 0.171 command-approval card through the core admission to
// the native decision and the by-link resolution runs here, against a
// published card, not against the file writer alone.
type linkChainSource struct {
	dir  string
	take bool // the share takes link answers now
}

func (s *linkChainSource) Select(rec *requests.Record) *protocol.Interaction {
	open, err := dialog.Requests(s.dir)
	if err != nil {
		return nil
	}
	for _, r := range open {
		if !r.Complete || r.Session == "" || r.Session != rec.NativeSession {
			continue
		}
		in, ok := dialog.RenderLinkInteraction(r)
		if !ok {
			continue
		}
		// The buttons are live while the dialog holds no answer file and
		// the share takes link answers; the core treats the answer file,
		// not the flag, as the truth at answer time.
		in.RemoteAnswer = s.take && !s.HasAnswer("", r.ID)
		return in
	}
	return nil
}

func (s *linkChainSource) Outcome(binding, id string) (protocol.Resolution, bool) {
	if r, ok, _ := dialog.ReadResolved(s.dir, id); ok {
		return linkChainResolution(r), true
	}
	return protocol.Resolution{}, false
}

func (s *linkChainSource) Present(binding, id string) (present bool, res protocol.Resolution, resolved bool) {
	_, _, _, r, err := dialog.Lookup(s.dir, id)
	if err != nil {
		return false, protocol.Resolution{}, false
	}
	if r != nil {
		return false, linkChainResolution(*r), true
	}
	return true, protocol.Resolution{}, false
}

func (s *linkChainSource) AnswerPolicy(host, binding string) (bool, string) {
	return s.take, ""
}

func (s *linkChainSource) RequestLive(rec *requests.Record) bool { return true }

func (s *linkChainSource) HasAnswer(binding, id string) bool {
	_, _, ans, resolved, err := dialog.Lookup(s.dir, id)
	return err == nil && (ans != nil || resolved != nil)
}

// WriteAnswer is the real pinned write, mapped the way the production
// seam maps it.
func (s *linkChainSource) WriteAnswer(host, bindingName, id, label, role, interactionDigest, documentDigest, credential string) (protocol.Code, string) {
	switch err := dialog.WriteLinkAnswer(s.dir, id, label, role, interactionDigest, documentDigest, credential); {
	case err == nil:
		return "", ""
	case errors.Is(err, dialog.ErrLinkDialogChanged):
		return protocol.CodeDialogChanged, err.Error()
	case errors.Is(err, dialog.ErrLinkDialogGone), errors.Is(err, dialog.ErrLinkAnswered):
		return protocol.CodeAlreadyResolved, err.Error()
	case errors.Is(err, dialog.ErrLinkNoSuchChoice):
		return protocol.CodeInvalid, err.Error()
	default:
		return protocol.CodeInternal, err.Error()
	}
}

func linkChainResolution(r dialog.Resolved) protocol.Resolution {
	out := protocol.Resolution{InteractionID: r.ID, Option: r.Label}
	switch r.By {
	case dialog.ByLink:
		out.Outcome = protocol.ResolutionAnswered
	default:
		out.Outcome = protocol.ResolutionElsewhere
	}
	return out
}

// linkChainEndpoint builds the core face the chain runs through: an
// endpoint over an open store with the chain source installed, and the
// signed link source its requests carry.
func linkChainEndpoint(t *testing.T, h *dialogHarness) (core.Source, *core.Endpoint) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close() })
	ep.SetConsent(func(string, string) bool { return true })
	ep.Register(fake.New("t1", "t1")) // the target the link shares: the fake's native id is its target, the dialog's session
	ep.SetLinkDialogs(&linkChainSource{dir: h.dir, take: true})
	// The fake app-server's native id is t1, the binding's native session.
	return core.Source{Host: "srv_cxchain", Binding: "cx", Shared: []string{"t1"}, Origin: map[string]string{"carrier": core.CarrierLink},
		NativeSession: "t1", Credential: "k1"}, ep
}

func linkChainSubmit(t *testing.T, ep *core.Endpoint, linkSrc core.Source) string {
	t.Helper()
	id := "11111111-1111-4111-8111-1111111111c6"
	rep, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "t1", Epoch: "t1",
		NotAfter: protocol.FormatTime(time.Now().Add(10 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"}}, linkSrc)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	rpl, _ := rep.(protocol.Reply)
	if rpl.Outcome.Code != "" {
		t.Fatalf("submit refused: %s %s", rpl.Outcome.Code, rpl.Outcome.Message)
	}
	return id
}

func linkChainGet(t *testing.T, ep *core.Endpoint, linkSrc core.Source, ref string) *protocol.Interaction {
	t.Helper()
	rep, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}, linkSrc)
	if err != nil {
		t.Fatalf("request.get: %v", err)
	}
	return rep.(protocol.Reply).Snapshot.Interaction
}

func linkChainWaitResolved(t *testing.T, dir, id string) dialog.Resolved {
	t.Helper()
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r, ok, _ := dialog.ReadResolved(dir, id); ok {
			return r
		}
	}
	t.Fatal("the dialog never resolved")
	return dialog.Resolved{}
}

// The chain both reviews asked for, on the plain command approval of the
// 0.171 schema (the fixture PR 1624 introduced): the card opens complete
// and actionable, the linked server reads it published with
// remote_answer true, the signed choice goes through AnswerLinkDialog,
// the attachment answers the app-server with Codex's own decision, and
// the dialog resolves by link.
func TestLinkAnswerCommandApprovalChain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		option   string
		decision string
		outcome  string
	}{
		{"allow once", "Allow once", "accept", dialog.OutcomeAllowed},
		{"decline", "Deny", "decline", dialog.OutcomeDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDialogHarness(t, "cx")
			linkSrc, ep := linkChainEndpoint(t, h)
			id := linkChainSubmit(t, ep, linkSrc)
			h.srv.sendServerRequest(t, "7", methodCommandApproval, plainCommandApprovalParams)
			req := h.opened("")
			if err := ep.Reconcile(); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			ref := protocol.EncodeRef(linkSrc.Host, "t1", id)
			in := linkChainGet(t, ep, linkSrc, ref)
			if in == nil || !in.RemoteAnswer || in.InteractionID != req.ID {
				t.Fatalf("published interaction = %+v; want the card live for %s", in, req.ID)
			}
			digest, err := core.InteractionDigest(in)
			if err != nil {
				t.Fatal(err)
			}
			code, msg := ep.AnswerLinkDialog(linkSrc, core.LinkAnswer{
				RequestRef:        ref,
				InteractionID:     in.InteractionID,
				InteractionDigest: digest,
				Option:            tc.option,
				DocumentDigest:    "sha256:doc",
				Credential:        "k1",
			})
			if code != "" {
				t.Fatalf("answer refused: %s %s", code, msg)
			}
			if got := h.reply("7"); got != `{"decision":"`+tc.decision+`"}` {
				t.Fatalf("response = %s, want %s", got, tc.decision)
			}
			r := linkChainWaitResolved(t, h.dir, req.ID)
			if r.Outcome != tc.outcome || r.Label != tc.option || r.By != dialog.ByLink {
				t.Fatalf("resolution = %+v; want %s %q by link", r, tc.outcome, tc.option)
			}
		})
	}
}

// The signature covers the record as the dialog directory holds it now,
// not as publication stored it: the record file edited after publication
// passes the core's check against its stored interaction but refuses at
// the write's pinned re-render with dialog_changed, and nothing is
// recorded.
func TestLinkAnswerCommandApprovalChainRejectsChangedRecord(t *testing.T) {
	h := newDialogHarness(t, "cx")
	linkSrc, ep := linkChainEndpoint(t, h)
	id := linkChainSubmit(t, ep, linkSrc)
	h.srv.sendServerRequest(t, "7", methodCommandApproval, plainCommandApprovalParams)
	req := h.opened("")
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ref := protocol.EncodeRef(linkSrc.Host, "t1", id)
	in := linkChainGet(t, ep, linkSrc, ref)
	if in == nil || !in.RemoteAnswer {
		t.Fatalf("published interaction = %+v; want the card live", in)
	}
	digest, err := core.InteractionDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	// Edit the record file after publication: the stored interaction in
	// the store still matches the signed digest, the record file does not.
	path := filepath.Join(h.dir, "requests", req.ID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recFile dialog.Request
	if err := json.Unmarshal(raw, &recFile); err != nil {
		t.Fatal(err)
	}
	recFile.Text += "\nsudo make me a sandwich"
	edited, err := json.Marshal(recFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	code, msg := ep.AnswerLinkDialog(linkSrc, core.LinkAnswer{
		RequestRef:        ref,
		InteractionID:     req.ID,
		InteractionDigest: digest,
		Option:            "Allow once",
		DocumentDigest:    "sha256:doc",
		Credential:        "k1",
	})
	if code != protocol.CodeDialogChanged {
		t.Fatalf("edited record = %s %s; want dialog_changed", code, msg)
	}
	// Nothing was recorded.
	if _, err := os.Stat(filepath.Join(h.dir, "answers", req.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("an answer was recorded: %v", err)
	}
}
