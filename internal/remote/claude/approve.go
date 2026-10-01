package claude

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Tool approvals from the Buzz DM (bead 611.42.3). The PermissionRequest
// hook and the attachment meet in files, because the hook process has only
// the Claude home:
//
//	~/.claude/sessions/amq-approve/<sid>/
//	  pin-<token>          DM edge: a relay share with approve is serving
//	  runs/<prompt_id>     attachment: an AMQ run owns this prompt
//	  requests/<iid>.json  hook: one open PermissionRequest
//	  answers/<iid>.json   attachment (Respond): the Buzz answer
//	  resolved/<iid>.json  whoever closed it first: hook or attachment
//
// Every directory is 0700 and every file 0600. Every read is no-follow and
// bounded; every write is create-new with its whole content, so the first
// writer of a resolved file decides how the interaction ended.

// approvalProtocol names the request file schema.
const approvalProtocol = "amq-claude-approval/v1"

// optionDeny is the one option a Claude approval offers from Buzz: Buzz can
// block a tool call and cannot allow one (owner ruling on bead 611.42.3).
// The deny decision never carries interrupt, so the turn continues.
const optionDeny = "deny"

// maxApprovalFileBytes bounds every approval file read. A request holds a
// preview of at most protocol.MaxApprovalPreview bytes plus small fields.
const maxApprovalFileBytes = 64 << 10

// promptIDRe bounds a prompt id before it becomes a file name.
var promptIDRe = sessionIDRe

// interactionIDRe is the id the hook mints: "cc-" and 32 hex characters.
var interactionIDRe = regexp.MustCompile(`^cc-[0-9a-f]{32}$`)

func approveDir(home, sessionID string) string {
	return filepath.Join(claudeSessionsDir(home), "amq-approve", sessionID)
}

// approvalRequest is requests/<iid>.json.
type approvalRequest struct {
	Protocol      string `json:"protocol"`
	InteractionID string `json:"interaction_id"`
	SessionID     string `json:"session_id"`
	PromptID      string `json:"prompt_id"`
	ToolName      string `json:"tool_name"`
	Preview       string `json:"preview"`
	ActionHash    string `json:"action_hash"`
	HookPID       int    `json:"hook_pid"`
	// OpenedAt is when the hook raised the request; a tool_result stamped
	// earlier answers an earlier call.
	OpenedAt string `json:"opened_at"`
	Deadline string `json:"deadline"`
}

// approvalAnswer is answers/<iid>.json.
type approvalAnswer struct {
	InteractionID string `json:"interaction_id"`
	ActionHash    string `json:"action_hash"`
	Option        string `json:"option"`
	At            string `json:"at"`
}

// approvalResolved is resolved/<iid>.json.
type approvalResolved struct {
	InteractionID string                     `json:"interaction_id"`
	Outcome       protocol.ResolutionOutcome `json:"outcome"`
	Option        string                     `json:"option,omitempty"`
}

// actionHash binds an answer to one exact tool call: SHA-256 over the tool
// name and the canonical tool input. The hook hashes its stdin tool_input
// and the attachment hashes the transcript tool_use input with this same
// function, so both sides agree byte for byte.
func actionHash(toolName string, input json.RawMessage) (string, bool) {
	canon, ok := canonicalJSON(input)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte("amq-remote/claude/action\x00" + toolName + "\x00" + string(canon)))
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

// canonicalJSON re-encodes a JSON value with sorted object keys and number
// literals kept as written.
func canonicalJSON(raw json.RawMessage) ([]byte, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("null")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	return out, err == nil
}

// bashFields are the Bash tool_input fields the preview renders by name.
// A call with any other field (dangerouslyDisableSandbox, for example) is
// shown as its whole input instead.
var bashFields = map[string]bool{"command": true, "description": true, "timeout": true, "run_in_background": true}

// approvalPreview renders what the DM shows for one PermissionRequest, with
// obvious secrets masked. Buzz can only block a Claude tool call (owner
// ruling on bead 611.42.3: a forged block is only a denial), so the preview
// grants nothing; it shows the owner what a ❌ would stop.
func approvalPreview(toolName string, input json.RawMessage, agentType string) string {
	var b strings.Builder
	if agentType != "" {
		fmt.Fprintf(&b, "Subagent %s asks:\n", oneLine(agentType))
	}
	if bash, ok := bashCall(input); toolName == "Bash" && ok {
		b.WriteString("Bash command:\n" + bash.Command)
		if bash.Description != "" {
			b.WriteString("\n\nDescription: " + oneLine(bash.Description))
		}
		if bash.Timeout != "" {
			b.WriteString("\nTimeout: " + bash.Timeout + " ms")
		}
		if bash.Background {
			b.WriteString("\nRuns in the background.")
		}
	} else {
		fmt.Fprintf(&b, "Tool %s:\n%s", oneLine(toolName), maskedJSON(input))
	}
	masked, _ := maskSecrets(b.String())
	preview, _ := protocol.TruncateText(masked, protocol.MaxApprovalPreview)
	return preview
}

type bashInput struct {
	Command, Description, Timeout string
	Background                    bool
}

// bashCall decodes a Bash tool_input that holds only the named fields,
// with their documented types.
func bashCall(input json.RawMessage) (bashInput, bool) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return bashInput{}, false
	}
	var out bashInput
	for k, v := range fields {
		if !bashFields[k] {
			return bashInput{}, false
		}
		var err error
		switch k {
		case "command":
			err = json.Unmarshal(v, &out.Command)
		case "description":
			err = json.Unmarshal(v, &out.Description)
		case "timeout":
			var n json.Number
			d := json.NewDecoder(bytes.NewReader(v))
			d.UseNumber()
			if err = d.Decode(&n); err == nil {
				out.Timeout = n.String()
			}
		case "run_in_background":
			err = json.Unmarshal(v, &out.Background)
		}
		if err != nil {
			return bashInput{}, false
		}
	}
	return out, true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// secretPatterns are the obvious secret shapes the masker hides: known
// token prefixes, private key blocks, bearer credentials, credentials in a
// URL, and a value assigned to a secret-looking name or flag. It is
// deliberately broad: a false positive hides harmless text, never leaks.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|glpat-[A-Za-z0-9_-]{20,})`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`),
	// Quoted values first, so a value with spaces is masked whole. An
	// unterminated quote masks to the end of the text.
	regexp.MustCompile(`(?i)\b` + secretName + `"?\s*[=:]\s*` + quotedValue),
	regexp.MustCompile(`(?i)\b` + secretName + `"?\s*[=:]\s*[^\s'"]+`),
	regexp.MustCompile(`(?i)--` + secretFlag + `(=|\s+)` + quotedValue),
	regexp.MustCompile(`(?i)--` + secretFlag + `(=|\s+)\S+`),
}

// secretName is a name that holds a secret: an assignment (API_KEY=), a
// JSON field ("password":), or a config key. secretFlag is such a flag.
// quotedValue is a double-quoted value with escapes or a single-quoted one.
const (
	secretName  = `[A-Z0-9_-]*(password|passwd|pwd|secret|token|api_?key|apikey|access_?key|private_?key|credentials?|auth)[A-Z0-9_-]*`
	secretFlag  = `(password|passwd|token|secret|api-key|apikey|access-key|auth)`
	quotedValue = `("(?:[^"\\]|\\.)*("|$)|'[^']*('|$))`
)

// secretKeyRe is a JSON field name whose value is a secret.
var secretKeyRe = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api_?key|apikey|access_?key|private_?key|credential|auth)`)

// maskedJSON renders a tool input for the DM with secrets hidden before it
// is encoded: every value under a secret-looking key, at any depth, becomes
// [masked], and every other string is masked as text. Masking the encoded
// JSON instead would miss quoted values, whose quotes JSON escapes.
func maskedJSON(input json.RawMessage) []byte {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []byte("[input not shown]")
	}
	out, err := json.Marshal(maskValue(v))
	if err != nil {
		return []byte("[input not shown]")
	}
	return out
}

func maskValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretKeyRe.MatchString(k) && val != nil {
				t[k] = "[masked]"
				continue
			}
			t[k] = maskValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = maskValue(val)
		}
		return t
	case string:
		masked, _ := maskSecrets(t)
		return masked
	}
	return v
}

// maskSecrets replaces every secret match with [masked] and reports whether
// anything changed.
func maskSecrets(s string) (string, bool) {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, "[masked]")
	}
	return out, out != s
}

// newInteractionID mints "cc-" and 32 random hex characters.
func newInteractionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "cc-" + hex.EncodeToString(b[:]), nil
}

// ensurePlainDir creates dir (0700) under a parent that must already be a
// plain directory, and refuses a symlink or non-directory at dir.
func ensurePlainDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a plain directory", dir)
	}
	return nil
}

// ensureApproveSubdir creates amq-approve/<sid>/<sub> one plain level at a
// time. sub "" is the session directory itself.
func ensureApproveSubdir(home, sessionID, sub string) (string, error) {
	if !noFollowSupported {
		return "", errUnsupportedPlatform
	}
	if !sessionIDRe.MatchString(sessionID) {
		return "", fmt.Errorf("session %q is not a file-safe id", sessionID)
	}
	sessions := claudeSessionsDir(home)
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return "", err
	}
	dirs := []string{filepath.Join(sessions, "amq-approve"), approveDir(home, sessionID)}
	if sub != "" {
		dirs = append(dirs, filepath.Join(approveDir(home, sessionID), sub))
	}
	for _, d := range dirs {
		if err := ensurePlainDir(d); err != nil {
			return "", err
		}
	}
	return dirs[len(dirs)-1], nil
}

// errFileExists is a create-new write that found the name taken.
var errFileExists = errors.New("file already exists")

// createNew writes data as dir/name only if the name is free, with its whole
// content visible at once: the bytes go to a private temporary file that is
// then hard-linked into place, which fails when the name exists.
func createNew(dir, name string, data []byte) error {
	if !noFollowSupported {
		return errUnsupportedPlatform
	}
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp, filepath.Join(dir, name)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errFileExists
		}
		return err
	}
	return nil
}

// createNewJSON is createNew for one JSON record.
func createNewJSON(dir, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return createNew(dir, name, raw)
}

// readApprovalJSON reads one bounded approval file into v. A missing file
// is os.ErrNotExist.
func readApprovalJSON(path string, v any) error {
	raw, err := readRegularBounded(path, maxApprovalFileBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// outcomeHookClaim is the hook's resolved record: its exclusive claim to
// answer. Only its delivery record makes that claim an answer. It never
// reaches the endpoint.
const outcomeHookClaim protocol.ResolutionOutcome = "hook_claimed"

// approvalDelivery is delivery/<iid>.json: whether the hook wrote its whole
// deny decision to Claude.
type approvalDelivery struct {
	InteractionID string `json:"interaction_id"`
	Written       bool   `json:"written"`
}

// writeDelivery records, create-new, whether the claiming hook's decision
// was written whole.
func writeDelivery(home, sessionID, id string, written bool) error {
	dir, err := ensureApproveSubdir(home, sessionID, "delivery")
	if err != nil {
		return err
	}
	return createNewJSON(dir, id+".json", approvalDelivery{InteractionID: id, Written: written})
}

// readDelivery reads delivery/<iid>.json.
func readDelivery(home, sessionID, id string) (approvalDelivery, bool) {
	var d approvalDelivery
	err := readApprovalJSON(filepath.Join(approveDir(home, sessionID), "delivery", id+".json"), &d)
	return d, err == nil && d.InteractionID == id
}

// writeResolved records how one interaction ended, create-new: the first
// writer wins and a later one gets errFileExists.
func writeResolved(home, sessionID string, r approvalResolved) error {
	dir, err := ensureApproveSubdir(home, sessionID, "resolved")
	if err != nil {
		return err
	}
	return createNewJSON(dir, r.InteractionID+".json", r)
}

// ApprovalPin is the DM edge's pin file content.
type approvalPin struct {
	PID   int    `json:"pid"`
	Share string `json:"share"`
}

// PinApprovals marks the session as served by a relay share that answers
// approvals from the DM: the PermissionRequest hook decides nothing without
// a pin whose pid is alive. It returns the pin's token for UnpinApprovals.
func PinApprovals(home, sessionID, share string, pid int) (string, error) {
	dir, err := ensureApproveSubdir(home, sessionID, "")
	if err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	if err := createNewJSON(dir, "pin-"+token, approvalPin{PID: pid, Share: share}); err != nil {
		return "", err
	}
	return token, nil
}

// UnpinApprovals removes one pin. Other pins for the session stay.
func UnpinApprovals(home, sessionID, token string) {
	if !sessionIDRe.MatchString(sessionID) || !noFollowSupported || token == "" || strings.ContainsAny(token, `/\`) {
		return
	}
	removeRegular(filepath.Join(approveDir(home, sessionID), "pin-"+token))
}

// pinLive reports whether the session has a pin whose serving pid is alive.
func pinLive(home, sessionID string) bool {
	dir := approveDir(home, sessionID)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), "pin-") {
			continue
		}
		var p approvalPin
		if readApprovalJSON(filepath.Join(dir, e.Name()), &p) != nil {
			continue
		}
		if alive, err := pidAlive(p.PID); err == nil && alive {
			return true
		}
	}
	return false
}

// runMarkerPath is runs/<prompt_id>.
func runMarkerPath(home, sessionID, promptID string) string {
	return filepath.Join(approveDir(home, sessionID), "runs", promptID)
}

// writeRunMarker records that an AMQ run owns promptID. An existing marker
// is kept.
func writeRunMarker(home, sessionID, promptID, msgID string) error {
	if !promptIDRe.MatchString(promptID) {
		return fmt.Errorf("prompt id %q is not a file-safe id", promptID)
	}
	dir, err := ensureApproveSubdir(home, sessionID, "runs")
	if err != nil {
		return err
	}
	err = createNewJSON(dir, promptID, map[string]string{"msg_id": msgID})
	if errors.Is(err, errFileExists) {
		return nil
	}
	return err
}

// removeRunMarker drops runs/<prompt_id>, so the hook decides nothing for
// that prompt any more.
func removeRunMarker(home, sessionID, promptID string) {
	if !sessionIDRe.MatchString(sessionID) || !promptIDRe.MatchString(promptID) || !noFollowSupported {
		return
	}
	removeRegular(runMarkerPath(home, sessionID, promptID))
}

// runMarked reports whether runs/<prompt_id> is a regular file.
func runMarked(home, sessionID, promptID string) bool {
	fi, err := os.Lstat(runMarkerPath(home, sessionID, promptID))
	return err == nil && fi.Mode().IsRegular()
}
