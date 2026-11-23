package linkio

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The carrier sends the verified choice to the endpoint. This fixture holds
// the share policy and models its answer slot with real dialog files.
type wireAnswerBinding struct {
	available              atomic.Bool
	loseReply              atomic.Bool
	server                 *fakeServer
	dir                    string
	host, binding, session string
	want                   core.LinkAnswer
}

func (b *wireAnswerBinding) LinkAnswerInfo(src core.Source, binding string) (bool, string) {
	if src.Host != b.host || src.Binding != b.binding || binding != b.binding || !b.available.Load() {
		return false, "approvals_terminal"
	}
	return true, ""
}

func (b *wireAnswerBinding) AnswerLinkDialog(src core.Source, answer core.LinkAnswer) (protocol.Code, string) {
	if ok, reason := b.LinkAnswerInfo(src, src.Binding); !ok {
		return protocol.CodeAnswerUnsupported, reason
	}
	if src.NativeSession != b.session || src.Credential != b.want.Credential || answer != b.want {
		return protocol.CodeInvalid, "verified answer fields differ"
	}
	won, err := dialog.WriteAnswer(b.dir, dialog.Answer{
		ID: answer.InteractionID, Option: 1, By: dialog.ByLink, DocumentDigest: answer.DocumentDigest, Credential: answer.Credential,
	})
	if err != nil {
		return protocol.CodeInternal, err.Error()
	}
	if !won {
		return protocol.CodeAlreadyResolved, "the interaction is no longer pending"
	}
	if b.loseReply.Swap(false) {
		// The file is durable but this connection cannot carry the reply.
		b.server.close(closeServerRestart)
	}
	return "", ""
}

func TestSignedAnswerOverLink(t *testing.T) {
	view, esKey := goldenView(t)
	var edKey ConsentKey
	if err := json.Unmarshal(readLinkFixture(t, "consent/assertion_ed25519.json"), &edKey); err != nil {
		t.Fatal(err)
	}
	var d answerDocument
	if err := json.Unmarshal(readLinkFixture(t, "answer/document.json"), &d); err != nil {
		t.Fatal(err)
	}
	b := Binding{Binding: d.Binding, TargetID: d.TargetID, Epoch: d.Epoch, NativeSessionID: d.NativeSessionID, Labels: d.Labels, Harness: "codex", Consent: "passkey"}
	fs, clk := newFakeServer(t), newClock()
	now, err := time.Parse(time.RFC3339, d.IssuedAt)
	if err != nil {
		t.Fatal(err)
	}
	clk.ns.Store(now.UnixNano())
	host, _, _, err := protocol.DecodeRef(d.RequestRef)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, sub := range []string{"requests", "answers", "resolved"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(readLinkFixture(t, "answer/document.json"))
	endpoint := &wireAnswerBinding{
		server: fs, dir: dir, host: host, binding: b.Binding, session: b.NativeSessionID,
		want: core.LinkAnswer{RequestRef: d.RequestRef, InteractionID: d.InteractionID, InteractionDigest: d.InteractionDigest, Option: d.Option, DocumentDigest: "sha256:" + hex.EncodeToString(sum[:]), Credential: edKey.CredentialID}}
	if err := dialog.Open(dir, dialog.Request{ID: d.InteractionID, Options: []dialog.Option{{N: 1, Label: d.Option, Scope: dialog.ScopeOnce}}}); err != nil {
		t.Fatal(err)
	}
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
		seed := sha256.Sum256([]byte("amq.remote.link/1 test device key"))
		private := ed25519.NewKeyFromSeed(seed[:])
		public, err := x509.MarshalPKIXPublicKey(private.Public())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Key = DeviceKey{Private: private, SPKI: public}
		cfg.Answers = endpoint
		cfg.StoreID = view.StoreID
		cfg.ConsentKeys = func() ([]ConsentKey, error) { return []ConsentKey{edKey, esKey}, nil }
		cfg.Bindings = func() []Binding { return []Binding{b} }
	})
	gen := fs.waitWelcome()
	waitOnline(t, c)
	exchange := func(t *testing.T, body json.RawMessage) Frame {
		t.Helper()
		fs.send(Frame{Schema: SchemaFrame, ID: "m_answer", Gen: gen, Body: body})
		r := fs.next()
		if r.Re != "m_answer" || r.Gen != gen {
			t.Fatalf("unexpected reply %+v", r)
		}
		return r
	}
	// Check the published query, unavailable reply and signed assertion as a
	// server would send and receive them. Repeating the assertion records nothing.
	info := exchange(t, goldenFrame(t, "58-answer_info.json").Body)
	sameBody(t, info, "m_answer", "59-answer_info_unavailable.json")
	wantRefusal := func(t *testing.T, body json.RawMessage, code string) {
		t.Helper()
		r := exchange(t, body)
		var refusal errorReply
		if err := decodeStrict(r.Body, "", &refusal); err != nil || refusal.Error.Code != code {
			t.Fatalf("got %s, want %s", r.Body, code)
		}
	}
	wantRefusal(t, mustJSON(GrantInfoRequest{Schema: SchemaAnswerInfo, Binding: "unshared-example"}), "unshared")
	signed := goldenFrame(t, "60-signed_answer.json").Body
	wantRefusal(t, signed, CodeAnswerUnsupported)
	wantRefusal(t, signed, CodeAnswerUnsupported)
	endpoint.available.Store(true)
	sameBody(t, exchange(t, goldenFrame(t, "58-answer_info.json").Body), "m_answer", "62-answer_info_available.json")
	var sequences []struct {
		Name  string
		Steps []struct{ Request, Reply, Action string }
	}
	if err := json.Unmarshal(readLinkFixture(t, "answer/sequences.json"), &sequences); err != nil {
		t.Fatal(err)
	}
	for _, seq := range sequences {
		t.Run(seq.Name, func(t *testing.T) {
			for _, step := range seq.Steps {
				switch step.Action {
				case "":
				case "lose_reply":
					endpoint.loseReply.Store(true)
					fs.send(Frame{Schema: SchemaFrame, ID: "m_answer", Gen: gen, Body: goldenFrame(t, step.Request).Body})
					gen = fs.waitWelcome()
					waitOnline(t, c)
					continue
				case "seam_rejects":
					if err := dialog.RejectAnswer(dir, d.InteractionID); err != nil {
						t.Fatal(err)
					}
				case "buzz_answers":
					won, err := dialog.WriteAnswer(dir, dialog.Answer{ID: d.InteractionID, Option: 1, By: dialog.ByBuzz})
					if err != nil || !won {
						t.Fatalf("Buzz answer won=%v err=%v", won, err)
					}
				default:
					t.Fatalf("unknown sequence action %q", step.Action)
				}
				sameBody(t, exchange(t, goldenFrame(t, step.Request).Body), "m_answer", step.Reply)
				if step.Action == "buzz_answers" {
					_, _, a, _, err := dialog.Lookup(dir, d.InteractionID)
					if err != nil || a == nil || a.By != dialog.ByBuzz {
						t.Fatalf("winning answer=%+v err=%v", a, err)
					}
				}
			}
			// Each sequence starts from the same open approval.
			if err := dialog.RejectAnswer(dir, d.InteractionID); err != nil {
				t.Fatal(err)
			}
		})
	}

	clk.advance(61 * time.Second)
	wantRefusal(t, signed, CodeClockSkew)
	clk.ns.Store(now.UnixNano())
	var invalid SignedSubmit
	if err := json.Unmarshal(signed, &invalid); err != nil {
		t.Fatal(err)
	}
	invalid.CredentialID = esKey.CredentialID
	wantRefusal(t, mustJSON(invalid), CodeConsentInvalid)

	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		code string
	}{
		{"missing signed key", func(v map[string]any) { delete(v, "option") }, CodeConsentInvalid},
		{"unknown signed key", func(v map[string]any) { v["extra"] = true }, CodeConsentInvalid},
		{"another store", func(v map[string]any) { v["store_id"] = "other" }, CodeConsentInvalid},
		{"unshared binding", func(v map[string]any) { v["binding"] = "unshared-example" }, "unshared"},
		{"stale binding", func(v map[string]any) { v["epoch"] = "previous" }, "stale_epoch"},
		{"changed native session", func(v map[string]any) { v["native_session_id"] = "previous" }, "session_changed"},
		{"clock skew before expiry", func(v map[string]any) {
			v["issued_at"] = now.Add(-2 * time.Minute).Format(time.RFC3339)
			v["not_after"] = d.IssuedAt
		}, CodeClockSkew},
		{"malformed timestamp", func(v map[string]any) { v["issued_at"] = "2026-10-09T14:02:00,0Z" }, CodeConsentInvalid},
		{"lifetime exceeds two minutes", func(v map[string]any) { v["not_after"] = now.Add(121 * time.Second).Format(time.RFC3339) }, CodeConsentInvalid},
		{"at expiry", func(v map[string]any) {
			v["issued_at"] = now.Add(-time.Minute).Format(time.RFC3339)
			v["not_after"] = d.IssuedAt
		}, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values map[string]any
			if err := json.Unmarshal(readLinkFixture(t, "answer/document.json"), &values); err != nil {
				t.Fatal(err)
			}
			tc.edit(values)
			raw, err := jcs.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			frame := signDoc(t, esKey, raw)
			frame.Schema = SchemaSignedAnswer
			wantRefusal(t, mustJSON(frame), tc.code)
		})
	}
}
