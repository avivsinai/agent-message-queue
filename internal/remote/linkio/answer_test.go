package linkio

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
)

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
	c, _ := startCarrier(t, fs, clk, func(cfg *Config) {
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
