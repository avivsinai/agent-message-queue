package linkcontract

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

var linkDir = filepath.Join("..", "..", "..", "testdata", "link")

// TestGenerateFixtures writes testdata/link once. The files are golden: the
// ES256 signature is randomized, so regenerating changes them, and a server
// that mirrors them must copy them again. It runs only with
// AMQ_LINK_FIXTURES_WRITE=1.
func TestGenerateFixtures(t *testing.T) {
	if os.Getenv("AMQ_LINK_FIXTURES_WRITE") != "1" {
		t.Skip("set AMQ_LINK_FIXTURES_WRITE=1 to rewrite testdata/link")
	}
	ck := consentKey(t)
	dk := deviceKey()
	consentSPKI := spki(t, &ck.PublicKey)
	deviceSPKI := spki(t, dk.Public())
	host := creatorHost(deviceSPKI)
	credID := credentialID()

	// The consent document, defaults written out, in canonical bytes.
	labels := map[string]any{"device": "Example MacBook", "session": "pi · demo repo", "harness": "pi", "project": "demo"}
	command := map[string]any{
		"schema": protocol.SchemaCommand, "op": "request.submit", "request_id": requestID,
		"target_id": targetID, "epoch": epoch, "not_after": notAfter,
		"input": map[string]any{"text": taskText, "busy": "reject", "deliver": "turn", "min_evidence": "submitted"},
	}
	document := mustJCS(t, map[string]any{
		"schema": "amq.remote.consent/2", "server_id": serverID, "store_id": storeID,
		"binding": bindingName, "native_session_id": nativeID, "issued_at": issuedAt,
		"labels": labels, "command": command,
	})
	writeRaw(t, "consent/document.json", document)

	// The software authenticator's assertion over SHA-256(document).
	docSum := sha256.Sum256(document)
	challenge := b64.EncodeToString(docSum[:])
	clientData := []byte(`{"type":"webauthn.get","challenge":"` + challenge + `","origin":"` + origin + `","crossOrigin":false}`)
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append(rpHash[:], 0x05) // UP | UV; BE and BS clear (backup_eligible false)
	authData = binary.BigEndian.AppendUint32(authData, 0)
	clientSum := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authData...), clientSum[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, ck, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	assertion := map[string]any{
		"credential_id": credID, "spki": b64.EncodeToString(consentSPKI), "alg": -7,
		"rp_id": rpID, "origin": origin, "backup_eligible": false,
		"fingerprint":        fingerprint(t, credID, consentSPKI, -7, rpID, origin),
		"challenge":          challenge,
		"authenticator_data": b64.EncodeToString(authData),
		"client_data_json":   b64.EncodeToString(clientData),
		"signature":          b64.EncodeToString(sig),
	}
	writeJSON(t, "consent/assertion.json", assertion)

	nonce := helloNonce()
	writeJSON(t, "device.json", map[string]any{
		"device_key":   b64.EncodeToString(deviceSPKI),
		"creator_host": host,
	})

	binding := map[string]any{
		"binding": bindingName, "target_id": targetID, "epoch": epoch, "native_session_id": nativeID,
		"labels": labels, "harness": "pi", "display_name": "pi · demo repo", "attachment": "live",
		"consent": "passkey", "tools": "read",
		"capabilities": map[string]any{
			"inspect": true, "submit": true, "cancel_request": false, "answer_question": false,
			"approve_tool": false, "steer": false, "terminal": "unavailable",
		},
	}
	ref := protocol.EncodeRef(host, targetID, requestID)
	inputDigest := protocol.CommandDigest(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: requestID,
		TargetID: targetID, Epoch: epoch, NotAfter: notAfter,
		Input: &protocol.SubmitInput{Text: taskText, Busy: "reject", Deliver: "turn", MinEvidence: "submitted"},
	})
	run := "pi-run-81"
	snapRunning := map[string]any{
		"schema": protocol.SchemaRequest, "request_ref": ref, "request_id": requestID, "creator_host": host,
		"target_id": targetID, "epoch": epoch, "revision": 4, "state": "running",
		"input_digest": inputDigest, "not_after": notAfter, "native_run": run,
		"interaction": map[string]any{
			"interaction_id": "ix_3", "kind": "approval", "prompt": "git push origin fix/ingest",
			"options": []string{"Allow", "Block"}, "approve_option": "Allow", "reject_option": "Block",
		},
		"observed_at": "2026-10-09T14:03:10Z",
	}
	snapDone := map[string]any{
		"schema": protocol.SchemaRequest, "request_ref": ref, "request_id": requestID, "creator_host": host,
		"target_id": targetID, "epoch": epoch, "revision": 6, "state": "completed",
		"input_digest": inputDigest, "not_after": notAfter, "native_run": run,
		"resolved":    []map[string]any{{"interaction_id": "ix_3", "outcome": "answered_elsewhere"}},
		"result":      map[string]any{"text": "Rebased cleanly. 412 passed, 1 skipped.", "truncated": false},
		"observed_at": "2026-10-09T14:06:02Z",
	}
	digest := func(snap map[string]any) string {
		sum := sha256.Sum256(mustJCS(t, snap))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	const gen = 12
	env := func(body any, kv ...any) map[string]any {
		f := map[string]any{"schema": "amq.remote.link/1", "body": body}
		for i := 0; i < len(kv); i += 2 {
			f[kv[i].(string)] = kv[i+1]
		}
		return f
	}
	frames := []struct {
		name, direction, def, description string
		frame                             map[string]any
	}{
		{"challenge", "server_to_endpoint", "challenge", "First frame after the socket opens. No gen yet.",
			env(map[string]any{"schema": "amq.remote.link.challenge/1", "server_id": serverID, "nonce": nonce}, "id", "m_c1")},
		{"hello", "endpoint_to_server", "hello", "Reply to the challenge; it carries its own id because welcome answers it. signature is Ed25519 over helloMessage (README).",
			env(map[string]any{
				"schema": "amq.remote.link.hello/1", "device_key": b64.EncodeToString(deviceSPKI), "store_id": storeID,
				"amq_version": "0.95.0", "signature": b64.EncodeToString(ed25519.Sign(dk, helloMessage(serverID, nonce, storeID))),
				"bindings": []any{binding}, "pending_local": []string{},
			}, "id", "m_h1", "re", "m_c1")},
		{"welcome", "server_to_endpoint", "welcome", "Reply to hello. From here every frame carries gen; a frame from an older generation is dropped.",
			env(map[string]any{
				"schema": "amq.remote.link.welcome/1", "user": "example.user", "connection_generation": gen,
				"limits": map[string]any{"frame_bytes": 4 << 20, "tasks_in_flight": 4, "tool_calls_in_flight": 8},
			}, "re", "m_h1", "gen", gen)},
		{"signed_submit", "server_to_endpoint", "signed_submit", "The signed document bytes and the assertion of consent/assertion.json.",
			env(map[string]any{
				"schema": "amq.remote.link.signed_submit/1", "document_b64": b64.EncodeToString(document),
				"credential_id": credID, "authenticator_data": assertion["authenticator_data"],
				"client_data_json": assertion["client_data_json"], "signature": assertion["signature"],
			}, "id", "m_s1", "gen", gen)},
		{"submit_outcome", "endpoint_to_server", "outcome_reply", "The reply to signed_submit is AMQ's Outcome, never a revision.",
			env(map[string]any{"outcome": map[string]any{"op": "request.submit", "evidence": "submitted"}}, "re", "m_s1", "gen", gen)},
		{"submit_refused", "endpoint_to_server", "error_reply", "A refusal before admission. The request ends with this reason.",
			env(map[string]any{"error": map[string]any{"code": "session_changed", "message": "the native session behind pi-demo changed since you signed"}}, "re", "m_s1", "gen", gen)},
		{"command_get", "server_to_endpoint", "command", "request.get; the reply is a hint, never stored as a revision.",
			env(map[string]any{"schema": protocol.SchemaCommand, "op": "request.get", "request_ref": ref}, "id", "m_g1", "gen", gen)},
		{"get_reply", "endpoint_to_server", "command_reply", "Snapshot and Outcome, as AMQ's Reply.",
			env(map[string]any{"snapshot": snapRunning, "outcome": map[string]any{"op": "request.get"}}, "re", "m_g1", "gen", gen)},
		{"command_cancel", "server_to_endpoint", "command", "request.cancel, sent only after the submit's Outcome.",
			env(map[string]any{"schema": protocol.SchemaCommand, "op": "request.cancel", "request_ref": ref, "target_id": targetID, "epoch": epoch, "not_after": "2026-10-09T14:05:00Z"}, "id", "m_x1", "gen", gen)},
		{"cancel_reply", "endpoint_to_server", "command_reply", "cancel_requested stays until the harness confirms.",
			env(map[string]any{"snapshot": snapRunning, "outcome": map[string]any{"op": "request.cancel", "disposition": "cancel_requested"}}, "re", "m_x1", "gen", gen)},
		{"command_session_list", "server_to_endpoint", "command", "session.list; only bindings shared with this link are answered.",
			env(map[string]any{"schema": protocol.SchemaCommand, "op": "session.list"}, "id", "m_l1", "gen", gen)},
		{"session_list_reply", "endpoint_to_server", "bindings_reply", "The shared bindings, approve and answer capabilities masked.",
			env(map[string]any{"bindings": []any{binding}}, "re", "m_l1", "gen", gen)},
		{"revision_interaction", "endpoint_to_server", "revision", "A committed revision AMQ owes. digest = sha256 of the JCS form of snapshot.",
			env(map[string]any{
				"schema": "amq.remote.link.revision/1", "store_id": storeID, "request_ref": ref,
				"revision": 4, "digest": digest(snapRunning), "snapshot": snapRunning,
			}, "id", "m_e7", "gen", gen)},
		{"revision_completed", "endpoint_to_server", "revision", "The final revision; the result text is shown verbatim.",
			env(map[string]any{
				"schema": "amq.remote.link.revision/1", "store_id": storeID, "request_ref": ref,
				"revision": 6, "digest": digest(snapDone), "snapshot": snapDone,
			}, "id", "m_e9", "gen", gen)},
		{"revision_ack", "server_to_endpoint", "ack_reply", "Sent only after the insert commits. ok is the acknowledged digest.",
			env(map[string]any{"ok": digest(snapDone)}, "re", "m_e9", "gen", gen)},
		{"busy", "either", "error_reply", "A budget is full. The sender keeps the work and tries again after retry_after_ms.",
			env(map[string]any{"error": map[string]any{"code": "busy", "message": "inbound budget full", "retry_after_ms": 2000}}, "re", "m_e9", "gen", gen)},
		{"bindings_changed", "endpoint_to_server", "bindings", "The shared bindings changed (attach, detach, native session, epoch). No reply.",
			env(map[string]any{"schema": "amq.remote.link.bindings/1", "bindings": []any{binding}}, "id", "m_b2", "gen", gen)},
		{"key_revoked", "server_to_endpoint", "key_revoked", "A consent key was removed; the endpoint drops it. No reply.",
			env(map[string]any{"schema": "amq.remote.link.key_revoked/1", "credential_id": credID}, "id", "m_k1", "gen", gen)},
		{"tools", "endpoint_to_server", "tools", "Ask for the tool catalog of this link's profile.",
			env(map[string]any{"schema": "amq.remote.link.tools/1"}, "id", "m_t1", "gen", gen)},
		{"tools_reply", "server_to_endpoint", "tools_reply", "The catalog. input_schema is the tool's JSON Schema.",
			env(map[string]any{"tools": []any{map[string]any{
				"name": "get_issue", "description": "Read one issue by key.", "write": false,
				"input_schema": map[string]any{
					"type": "object", "required": []string{"key"}, "additionalProperties": false,
					"properties": map[string]any{"key": map[string]any{"type": "string"}},
				},
			}}}, "re", "m_t1", "gen", gen)},
		{"call", "endpoint_to_server", "call", "A tool call. idempotency_key makes a retry return the same call.",
			env(map[string]any{
				"schema": "amq.remote.link.call/1", "call_id": "c_81", "tool": "get_issue",
				"arguments": map[string]any{"key": "REL-4410"}, "idempotency_key": "k_81", "binding": bindingName,
			}, "id", "m_c4", "gen", gen)},
		{"call_ok", "server_to_endpoint", "call_reply", "A read that finished.",
			env(map[string]any{"call_id": "c_81", "status": "ok", "result": map[string]any{"key": "REL-4410", "summary": "Ingest fails on empty batch"}}, "re", "m_c4", "gen", gen)},
		{"call_pending", "server_to_endpoint", "call_reply", "A read still running after 25 s; finish with call_get.",
			env(map[string]any{"call_id": "c_81", "status": "pending"}, "re", "m_c4", "gen", gen)},
		{"call_pending_approval", "server_to_endpoint", "call_reply", "A write waits for the owner's decision on the server.",
			env(map[string]any{"call_id": "c_82", "status": "pending_approval", "review_url": "https://app.example.test/calls/c_82"}, "re", "m_c5", "gen", gen)},
		{"call_error", "server_to_endpoint", "call_reply", "The call failed or was refused.",
			env(map[string]any{"call_id": "c_83", "status": "error", "error": map[string]any{"code": "not_allowed", "message": "get_salary is not in this link's tool profile"}}, "re", "m_c6", "gen", gen)},
		{"call_get", "endpoint_to_server", "call_get", "Wait up to wait_ms for the call's final state.",
			env(map[string]any{"schema": "amq.remote.link.call_get/1", "call_id": "c_82", "wait_ms": 30000}, "id", "m_c7", "gen", gen)},
		{"call_get_reply", "server_to_endpoint", "call_reply", "The final state of a decided write.",
			env(map[string]any{"call_id": "c_82", "status": "rejected"}, "re", "m_c7", "gen", gen)},
	}
	if err := os.RemoveAll(filepath.Join(linkDir, "frames")); err != nil {
		t.Fatal(err)
	}
	for i, f := range frames {
		writeJSON(t, fmt.Sprintf("frames/%02d-%s.json", i+1, f.name), map[string]any{
			"description": f.description, "direction": f.direction, "body_def": f.def, "frame": f.frame,
		})
	}
}

func mustJCS(t *testing.T, v any) []byte {
	t.Helper()
	b, err := jcs.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeRaw(t *testing.T, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(linkDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeJSON writes v indented, with <, > and & as they are.
func writeJSON(t *testing.T, rel string, v any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, rel, buf.Bytes())
}
