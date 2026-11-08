// Package acpwire is ACP's session/update wire form: the JSON-RPC
// notification an agent sends to stream text into a client session.
package acpwire

import "encoding/json"

// JSONRPCVersion is the JSON-RPC version every ACP message carries.
const JSONRPCVersion = "2.0"

// SessionUpdateNotification is one session/update notification.
type SessionUpdateNotification struct {
	JSONRPC string              `json:"jsonrpc"`
	Method  string              `json:"method"`
	Params  SessionUpdateParams `json:"params"`
}

// SessionUpdateParams addresses an update to one session. Meta is ACP
// extension metadata and is omitted when empty.
type SessionUpdateParams struct {
	SessionID string            `json:"sessionId"`
	Update    SessionUpdate     `json:"update"`
	Meta      map[string]string `json:"_meta,omitempty"`
}

// SessionUpdate is a text update of one kind, such as agent_message_chunk.
type SessionUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Content       TextContent `json:"content"`
	ToolCallID    string      `json:"toolCallId,omitempty"`
}

// TextContent is an ACP text content block.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// TextSessionUpdate is the session/update notification that streams text of
// updateType into sessionID.
func TextSessionUpdate(sessionID, updateType, text string) SessionUpdateNotification {
	return SessionUpdateNotification{
		JSONRPC: JSONRPCVersion,
		Method:  "session/update",
		Params: SessionUpdateParams{
			SessionID: sessionID,
			Update: SessionUpdate{
				SessionUpdate: updateType,
				Content:       TextContent{Type: "text", Text: text},
			},
		},
	}
}

// MarshalTextSessionUpdate encodes TextSessionUpdate with optional extension
// metadata and tool call id; each is omitted when empty.
func MarshalTextSessionUpdate(sessionID, updateType, text string, meta map[string]string, toolCallID string) (json.RawMessage, error) {
	note := TextSessionUpdate(sessionID, updateType, text)
	note.Params.Meta = meta
	note.Params.Update.ToolCallID = toolCallID
	return json.Marshal(note)
}
