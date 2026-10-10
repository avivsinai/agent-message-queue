// Package linkio is the link carrier: one WebSocket that amq-remote dials to
// a linked server, carrying the server's commands down and the revisions AMQ
// owes up (protocol amq.remote.link/1, schemas/remote-link-v1.schema.json).
// The carrier names no server; a server is whatever answers the challenge
// with the server id pinned at linking.
package linkio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Schema strings of amq.remote.link/1.
const (
	SchemaFrame        = "amq.remote.link/1"
	SchemaChallenge    = "amq.remote.link.challenge/1"
	SchemaHello        = "amq.remote.link.hello/1"
	SchemaWelcome      = "amq.remote.link.welcome/1"
	SchemaSignedSubmit = "amq.remote.link.signed_submit/1"
	SchemaRevision     = "amq.remote.link.revision/1"
	SchemaBindings     = "amq.remote.link.bindings/1"
	SchemaKeyRevoked   = "amq.remote.link.key_revoked/1"
)

// Frame is the envelope. id names a frame that expects a reply, re answers
// one; gen is absent on challenge and hello and present on every later frame.
// The envelope ignores unknown fields; bodies are decoded strictly.
type Frame struct {
	Schema string          `json:"schema"`
	ID     string          `json:"id,omitempty"`
	Re     string          `json:"re,omitempty"`
	Gen    int64           `json:"gen,omitempty"`
	Body   json.RawMessage `json:"body"`
}

// Labels are the machine's own names for a binding. A consent document
// carries them, and the server shows only these.
type Labels struct {
	Device  string `json:"device"`
	Session string `json:"session"`
	Harness string `json:"harness"`
	Project string `json:"project"`
}

// Binding is one AMQ binding shared with a link, as the machine sees it now.
type Binding struct {
	Binding         string                `json:"binding"`
	TargetID        string                `json:"target_id"`
	Epoch           string                `json:"epoch"`
	NativeSessionID string                `json:"native_session_id"`
	Labels          Labels                `json:"labels"`
	Harness         string                `json:"harness"`
	DisplayName     string                `json:"display_name"`
	Attachment      string                `json:"attachment"`
	Consent         string                `json:"consent"`
	Tools           string                `json:"tools"`
	Capabilities    protocol.Capabilities `json:"capabilities"`
}

type challengeBody struct {
	Schema   string `json:"schema"`
	ServerID string `json:"server_id"`
	Nonce    string `json:"nonce"`
}

type helloBody struct {
	Schema       string    `json:"schema"`
	DeviceKey    string    `json:"device_key"`
	StoreID      string    `json:"store_id"`
	AMQVersion   string    `json:"amq_version"`
	Signature    string    `json:"signature"`
	Bindings     []Binding `json:"bindings"`
	PendingLocal []string  `json:"pending_local"`
}

// Limits are the server's budgets, sent in welcome.
type Limits struct {
	FrameBytes        int64 `json:"frame_bytes"`
	TasksInFlight     int   `json:"tasks_in_flight"`
	ToolCallsInFlight int   `json:"tool_calls_in_flight"`
}

type welcomeBody struct {
	Schema               string `json:"schema"`
	User                 string `json:"user"`
	ConnectionGeneration int64  `json:"connection_generation"`
	Limits               Limits `json:"limits"`
}

type revisionBody struct {
	Schema     string            `json:"schema"`
	StoreID    string            `json:"store_id"`
	RequestRef string            `json:"request_ref"`
	Revision   int64             `json:"revision"`
	Digest     string            `json:"digest"`
	Snapshot   protocol.Snapshot `json:"snapshot"`
}

type bindingsBody struct {
	Schema   string    `json:"schema"`
	Bindings []Binding `json:"bindings"`
}

// bindingsReply answers session.list and session.inspect from a link.
type bindingsReply struct {
	Bindings []Binding `json:"bindings"`
}

type keyRevokedBody struct {
	Schema       string `json:"schema"`
	CredentialID string `json:"credential_id"`
}

// ErrorBody is a typed refusal. busy carries retry_after_ms.
type ErrorBody struct {
	Code         string `json:"code"`
	Message      string `json:"message,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

type errorReply struct {
	Error ErrorBody `json:"error"`
}

// reply is the union of the reply bodies the carrier itself reads: an
// acknowledgement or a refusal. Unknown keys are ignored here; each branch
// is checked strictly by its reader.
type reply struct {
	OK    string     `json:"ok"`
	Error *ErrorBody `json:"error"`
}

// bodySchema reads only the schema string of a body.
func bodySchema(raw json.RawMessage) string {
	var s struct {
		Schema string `json:"schema"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.Schema
}

// decodeStrict decodes one body, refusing unknown keys, duplicate keys and
// trailing data, and checks its schema string.
func decodeStrict(raw json.RawMessage, schema string, v any) error {
	if err := noDuplicateKeys(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after body")
	}
	if schema != "" {
		if got := bodySchema(raw); got != schema {
			return fmt.Errorf("body schema %q, want %q", got, schema)
		}
	}
	return nil
}

// noDuplicateKeys walks the JSON tokens and refuses an object that repeats
// a key: encoding/json would silently keep the last one.
func noDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object bool
		keys   map[string]bool
		key    bool // the next string token is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				if top != nil && top.object {
					top.key = true
				}
				stack = append(stack, &frame{object: t == '{', keys: map[string]bool{}, key: t == '{'})
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if top != nil && top.object && top.key {
				if top.keys[t] {
					return fmt.Errorf("duplicate key %q", t)
				}
				top.keys[t] = true
				top.key = false
				continue
			}
			if top != nil && top.object {
				top.key = true
			}
		default:
			if top != nil && top.object {
				top.key = true
			}
		}
	}
}
