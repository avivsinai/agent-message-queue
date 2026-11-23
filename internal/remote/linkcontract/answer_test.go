package linkcontract

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

func answerFrames(t *testing.T, host string, gen int) []goldenFrame {
	t.Helper()
	const binding = "codex-demo"
	const target = "codex:demo"
	const id = "d-5644edd335ce43efb10d5c640672479d"
	const choice = "Allow once"
	ref := protocol.EncodeRef(host, target, requestID)
	interaction := map[string]any{
		"interaction_id": id, "kind": "approval", "options": []string{"Decline", choice},
		"prompt": "Command\nprintf '<café> & 世界\u2028'", "approve_option": choice, "reject_option": "Decline",
	}
	projected := mustJCS(t, interaction)
	writeRaw(t, "answer/interaction.json", projected)
	h := sha256.Sum256(projected)
	doc := mustJCS(t, map[string]any{
		"schema": "amq.remote.answer/1", "server_id": serverID, "store_id": storeID,
		"binding": binding, "target_id": target, "epoch": epoch, "native_session_id": "codex-demo-session",
		"labels":      map[string]string{"device": "Example MacBook", "session": "Codex demo", "harness": "codex", "project": "demo"},
		"request_ref": ref, "interaction_id": id, "interaction_digest": "sha256:" + hex.EncodeToString(h[:]),
		"option": choice, "issued_at": issuedAt, "not_after": notAfter,
	})
	writeRaw(t, "answer/document.json", doc)
	sum := sha256.Sum256(doc)
	client := []byte(`{"type":"webauthn.get","challenge":"` + b64.EncodeToString(sum[:]) + `","origin":"` + origin + `","crossOrigin":false}`)
	rp := sha256.Sum256([]byte(rpID))
	auth := binary.BigEndian.AppendUint32(append(rp[:], 0x05), 0)
	clientSum := sha256.Sum256(client)
	signature := ed25519.Sign(consentKeyEd25519(), append(bytes.Clone(auth), clientSum[:]...))
	req := "server_to_endpoint"
	rep := "endpoint_to_server"
	frames := []goldenFrame{
		{"answer_info", req, "answer_info", "Ask whether the shared binding accepts signed answers now.", env(map[string]any{"schema": "amq.remote.link.answer_info/1", "binding": binding}, "id", "m_ai", "gen", gen)},
		{"answer_info_unavailable", rep, "answer_info_reply", "The share uses the default terminal approval policy.", env(map[string]any{"answer_info": map[string]any{"binding": binding, "available": false, "reason": "approvals_terminal", "now": issuedAt}}, "re", "m_ai", "gen", gen)},
		{"signed_answer", req, "signed_answer", "The enrolled Ed25519 test key signs answer/document.json.", env(map[string]any{
			"schema": "amq.remote.link.signed_answer/1", "document_b64": b64.EncodeToString(doc), "credential_id": credentialIDEd25519(),
			"authenticator_data": b64.EncodeToString(auth), "client_data_json": b64.EncodeToString(client), "signature": b64.EncodeToString(signature),
		}, "id", "m_sa", "gen", gen)},
		{"answer_recorded", rep, "answer_reply", "The machine durably recorded the signed choice.", env(map[string]any{"answer": map[string]any{"request_ref": ref, "interaction_id": id, "state": "recorded"}}, "re", "m_sa", "gen", gen)},
		{"answer_info_available", rep, "answer_info_reply", "The opted-in binding accepts signed one-time choices.", env(map[string]any{"answer_info": map[string]any{"binding": binding, "available": true, "now": issuedAt}}, "re", "m_ai", "gen", gen)},
	}
	for _, row := range []struct{ code, message string }{
		{"consent_invalid", "the answer is for another server or store"},
		{"unshared", `binding "codex-demo" is not shared with this link`},
		{"stale_epoch", "the binding changed since you signed; read it again and sign again"},
		{"session_changed", "the session behind codex-demo changed since you signed"},
		{"clock_skew", "check your clock: the consent was issued at 2026-10-09T14:02:00Z, this machine says 2026-10-09T14:05:00Z"},
		{"expired", "the consent expired at 2026-10-09T14:02:10Z"},
		{"answer_unsupported", `binding "codex-demo" takes answers at the terminal`},
		{"not_found", "no running request on this binding"},
		{"already_resolved", "the interaction is no longer pending"},
		{"dialog_changed", "the pending interaction has changed"},
		{"invalid", "the option is not a link choice"},
		{"internal", "the machine could not confirm that the answer was durably recorded"},
	} {
		frames = append(frames, goldenFrame{"answer_refused_" + row.code, rep, "error_reply", "The typed refusal for signed_answer. The message is illustrative.", env(map[string]any{"error": map[string]string{"code": row.code, "message": row.message}}, "re", "m_sa", "gen", gen)})
	}
	// A snapshot is immutable. Clone the signed projection before adding the
	// transient button state; every golden retains its own snapshot bytes.
	for _, r := range []struct {
		name       string
		revision   int
		actionable bool
		outcome    string
	}{
		{"display", 4, false, ""}, {"actionable", 5, true, ""}, {"occupied", 6, false, ""}, {"reopened", 7, true, ""},
		{"answered", 8, false, "answered"}, {"elsewhere", 8, false, "answered_elsewhere"}, {"ended", 8, false, "run_ended"}, {"unknown", 8, false, "unknown"},
	} {
		snap := map[string]any{"schema": protocol.SchemaRequest, "request_ref": ref, "creator_host": host, "target_id": target, "request_id": requestID, "epoch": epoch, "revision": r.revision, "state": "running", "native_run": "codex-turn-demo", "observed_at": issuedAt}
		if r.outcome == "" {
			card := maps.Clone(interaction)
			if r.actionable {
				card["remote_answer"] = true
			}
			snap["interaction"] = card
		} else {
			result := map[string]string{"interaction_id": id, "outcome": r.outcome}
			switch r.outcome {
			case "answered", "answered_elsewhere":
				result["option"] = choice
			case "unknown":
				snap["state"] = "completed"
				snap["result"] = map[string]any{"text": "Task completed.", "truncated": false}
			}
			snap["resolved"] = []map[string]string{result}
		}
		sum := sha256.Sum256(mustJCS(t, snap))
		body := map[string]any{"schema": "amq.remote.link.revision/1", "store_id": storeID, "request_ref": ref, "revision": r.revision, "digest": "sha256:" + hex.EncodeToString(sum[:]), "snapshot": snap}
		frames = append(frames, goldenFrame{"revision_answer_" + r.name, rep, "revision", "The linked permission dialog is " + r.name + ".", env(body, "id", fmt.Sprintf("m_dialog_%s", r.name), "gen", gen)})
	}
	return frames
}

// Servers can use the committed projection to reproduce the digest and pass
// the exact document bytes into WebAuthn, independently of the fixture writer.
func TestAnswerSigningBytes(t *testing.T) {
	doc := readFile(t, "answer/document.json")
	var d struct {
		InteractionDigest string `json:"interaction_digest"`
	}
	mustUnmarshal(t, doc, &d)
	var m struct {
		DocumentB64 string `json:"document_b64"`
	}
	mustUnmarshal(t, bodyOf(t, "signed_answer"), &m)
	var interaction map[string]json.RawMessage
	mustUnmarshal(t, readFile(t, "answer/interaction.json"), &interaction)
	sum := sha256.Sum256(mustJCS(t, interaction))
	if !bytes.Equal(doc, mustB64(t, m.DocumentB64)) || d.InteractionDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("answer document bytes or signed interaction digest differ")
	}
}
