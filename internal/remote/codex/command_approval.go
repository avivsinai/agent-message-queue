package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// A plain command has none of these optional contexts. Codex may omit
// them or explicitly send JSON null.
type nullApprovalField struct{}

func (*nullApprovalField) UnmarshalJSON(data []byte) error {
	if string(data) != "null" {
		return errors.New("additional command approval context")
	}
	return nil
}

// localEnvironment accepts the two ways Codex names a run on this machine:
// JSON null and the reserved id "local". Any other environment id can name
// a remote run, which the command and its directory do not describe.
type localEnvironment struct{}

func (*localEnvironment) UnmarshalJSON(data []byte) error {
	if s := string(data); s != "null" && s != `"local"` {
		return errors.New("command runs in another environment")
	}
	return nil
}

// commandApproval is the closed Codex command-approval envelope.
type commandApproval struct {
	threadID, turnID, itemID                      string
	startedAtMs                                   *int64
	kind, command, cwd, reason                    string
	commandActions                                []json.RawMessage
	availableDecisions                            []json.RawMessage
	approvalID                                    nullApprovalField
	environmentID                                 localEnvironment
	additionalPermissions, networkApprovalContext nullApprovalField
	proposedNetworkPolicyAmendments               nullApprovalField
	proposedExecpolicyAmendment                   execPrefix
}

// execPrefix is a proposed exec-policy prefix: JSON null, or a JSON array
// whose every element is a JSON string.
type execPrefix struct {
	set  bool
	args []string
}

func (e *execPrefix) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var raws []json.RawMessage
	if len(data) == 0 || data[0] != '[' || json.Unmarshal(data, &raws) != nil {
		return errors.New("exec-policy prefix is not an array")
	}
	args := make([]string, len(raws))
	for i, raw := range raws {
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &args[i]) != nil {
			return errors.New("exec-policy prefix element is not a string")
		}
	}
	e.set, e.args = true, args
	return nil
}

// amendmentDecision reports whether raw is the one object decision Codex
// offers with a proposed exec-policy amendment, naming exactly that prefix.
func amendmentDecision(raw json.RawMessage, proposed execPrefix) bool {
	var inner json.RawMessage
	if !proposed.set || !decodeApprovalFields(raw, map[string]any{"acceptWithExecpolicyAmendment": &inner}) || inner == nil {
		return false
	}
	var prefix execPrefix
	return decodeApprovalFields(inner, map[string]any{"execpolicy_amendment": &prefix}) &&
		prefix.set && slices.Equal(prefix.args, proposed.args)
}

// plainCommandText returns the complete decision text or an empty string.
// The envelope identifies the callback; command and cwd name the action.
func plainCommandText(params json.RawMessage) string {
	var p commandApproval
	if !decodeApprovalFields(params, map[string]any{
		"threadId": &p.threadID, "turnId": &p.turnID, "itemId": &p.itemID, "startedAtMs": &p.startedAtMs,
		"kind": &p.kind, "command": &p.command, "cwd": &p.cwd, "reason": &p.reason,
		"commandActions": &p.commandActions, "availableDecisions": &p.availableDecisions,
		"approvalId": &p.approvalID, "environmentId": &p.environmentID,
		"additionalPermissions": &p.additionalPermissions, "networkApprovalContext": &p.networkApprovalContext,
		"proposedExecpolicyAmendment":     &p.proposedExecpolicyAmendment,
		"proposedNetworkPolicyAmendments": &p.proposedNetworkPolicyAmendments,
	}) {
		return ""
	}
	if p.kind != "command" || p.threadID == "" || p.turnID == "" || p.itemID == "" || p.startedAtMs == nil ||
		strings.TrimSpace(p.command) == "" || strings.TrimSpace(p.cwd) == "" {
		return ""
	}
	for _, action := range p.commandActions {
		if !knownCommandAction(action) {
			return ""
		}
	}
	// Require a native one-time choice. The local renderer also offers
	// fallback choices when older servers omit their list.
	once := false
	seen := make(map[string]bool)
	// A proposed exec-policy amendment and its one decision come together
	// or not at all. A one-time accept approves this command and applies no
	// amendment, so the text omits it.
	amendments := 0
	for _, raw := range p.availableDecisions {
		var decision string
		if json.Unmarshal(raw, &decision) != nil {
			amendments++
			if amendments > 1 || !amendmentDecision(raw, p.proposedExecpolicyAmendment) {
				return ""
			}
			continue
		}
		if seen[decision] {
			return ""
		}
		seen[decision] = true
		switch decision {
		case "accept", "decline":
			once = true
		case "acceptForSession", "cancel":
		default:
			return ""
		}
	}
	if !once || p.proposedExecpolicyAmendment.set != (amendments == 1) {
		return ""
	}
	text := "$ " + p.command + "\nin " + p.cwd
	if p.reason != "" {
		text += "\nreason: " + p.reason
	}
	return text
}

// Command actions are Codex's best-effort display annotations derived from
// the original command. Only the known variants can accompany a plain approval.
func knownCommandAction(raw json.RawMessage) bool {
	var action struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &action) != nil {
		return false
	}
	var command, path, name, query *string
	fields := map[string]any{"type": &action.Type, "command": &command}
	switch action.Type {
	case "read":
		fields["name"], fields["path"] = &name, &path
	case "listFiles":
		fields["path"] = &path
	case "search":
		fields["path"], fields["query"] = &path, &query
	case "unknown":
	default:
		return false
	}
	return decodeApprovalFields(raw, fields) && command != nil &&
		(action.Type != "read" || (name != nil && path != nil))
}

// The caller supplies one valid JSON value and a fresh field table. Consume
// each exact native key once; Go struct decoding also accepts case aliases.
func decodeApprovalFields(raw json.RawMessage, fields map[string]any) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		dst, ok := fields[key]
		if !ok {
			return false
		}
		delete(fields, key)
		if d.Decode(dst) != nil {
			return false
		}
	}
	token, err := d.Token()
	return err == nil && token == json.Delim('}')
}
