package claude

import (
	"bytes"
	"context"
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
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/secretscan"
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
//	  rejected/<iid>-<proof>  hook: it refused one allow proof
//	  verdicts/<iid>-<proof>  hook: its one verdict on one allow proof
//
// Every directory is 0700 and every file 0600. Every read is no-follow and
// bounded; every write is create-new with its whole content, so the first
// writer of a resolved file decides how the interaction ended.

// approvalProtocol names the request file schema.
const approvalProtocol = "amq-claude-approval/v1"

// optionDeny blocks a Claude tool call from Buzz. Its decision never
// carries interrupt, so the turn continues. A forged deny is only a denial,
// so a deny needs no proof beyond the answer file (bead 611.42.3).
const optionDeny = "deny"

// optionAllow lets a Claude tool call run from Buzz. The hook applies it only
// with the owner's signed reaction on the approval message that showed this
// exact call (bead 611.42.4), never on the answer file alone.
const optionAllow = "allow"

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
	// Approvable means the hook can apply an allow for this call: it pins
	// an owner, and Preview is the signed prompt that shows the whole call.
	Approvable bool `json:"approvable,omitempty"`
	HookPID    int  `json:"hook_pid"`
	// OpenedAt is when the hook raised the request; a tool_result stamped
	// earlier answers an earlier call.
	OpenedAt string `json:"opened_at"`
	Deadline string `json:"deadline"`
}

// approvalAnswer is answers/<iid>.json. Evidence is the surface's proof
// for an allow, opaque here: the hook's verifier reads it.
type approvalAnswer struct {
	InteractionID string          `json:"interaction_id"`
	ActionHash    string          `json:"action_hash"`
	Option        string          `json:"option"`
	At            string          `json:"at"`
	Evidence      json.RawMessage `json:"evidence,omitempty"`
}

// maxEvidenceBytes bounds the evidence an answer carries, so the answer
// file stays within maxApprovalFileBytes.
const maxEvidenceBytes = 48 << 10

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

// approvalView renders what the DM shows for one PermissionRequest. The
// preview never edits a call: it shows the call whole, or a fixed note.
// The whole candidate text is built first, in the form the DM displays,
// and one detector checks that text and the structured input. On any
// match only the hidden note is returned; a call too long to show whole
// returns the too-long note. Every return is within MaxApprovalPreview.
//
// whole reports that the preview is a Bash call with only the named
// fields, shown exactly as it runs: nothing hidden, nothing shortened, and
// no character removed for display or replaced in decoding. Only such a
// call can be allowed from Buzz (bead 611.42.4): the owner approves the
// text they saw, so that text must be the call.
func approvalView(toolName string, input json.RawMessage, agentType string) (preview string, whole bool) {
	var b strings.Builder
	if agentType != "" {
		fmt.Fprintf(&b, "Subagent %s asks:\n", oneLine(agentType))
	}
	bash, isBash := bashCall(input)
	isBash = isBash && toolName == "Bash"
	if isBash {
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
		canon, _ := canonicalJSON(input)
		fmt.Fprintf(&b, "Tool %s:\n%s", oneLine(toolName), canon)
	}
	raw := b.String()
	candidate := secretscan.DisplayForm(raw)
	switch {
	case secretscan.MayHold(candidate) || inputMayHoldSecret(input):
		return bounded(previewHidden), false
	case len(candidate) > protocol.MaxApprovalPreview:
		return bounded(previewTooLong), false
	}
	whole = isBash && candidate == raw && !strings.ContainsRune(raw, utf8.RuneError)
	return bounded(candidate), whole
}

// signedPrompt is the prompt an approvable call shows: its whole preview,
// then the interaction id and the action hash. The approval message the
// owner reacts to carries it, and the hook compares that message with its
// own rendering, so the owner's signature covers this exact call. ok is
// false when it would not fit MaxApprovalPreview, which would cut it.
func signedPrompt(preview, interactionID, hash string) (string, bool) {
	p := preview + "\n\nInteraction: " + interactionID + "\nAction: " + hash
	return p, len(p) <= protocol.MaxApprovalPreview
}

// The notes a preview shows instead of a call it does not show whole.
const (
	previewHidden  = "Command hidden: it may contain a secret. Check the terminal."
	previewTooLong = "Command too long: check the terminal."
)

// bounded is s cut to MaxApprovalPreview, the bound every preview keeps.
func bounded(s string) string {
	out, _ := protocol.TruncateText(s, protocol.MaxApprovalPreview)
	return out
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

// inputMayHoldSecret checks the structured input: every decoded key by
// name, and every key and string value as text.
func inputMayHoldSecret(input json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return secretscan.MayHold(string(input))
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if secretscan.NameMayHold(k) || secretscan.MayHold(k) || walk(val) {
					return true
				}
			}
		case []any:
			for _, val := range t {
				if walk(val) {
					return true
				}
			}
		case string:
			return secretscan.MayHold(t)
		}
		return false
	}
	return walk(v)
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
// decision to Claude.
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

// rejected/<iid>-<proof> records that the hook could not verify one allow
// answer, named by the hash of its evidence: that answer never applies,
// a deny or a new proof replaces it, and the endpoint drops its intent
// (Pro review of #936 r2). A record names one exact proof, so it never
// retires a newer one.

// proofName is the rejected record's name for an allow's evidence.
func proofName(interactionID string, evidence json.RawMessage) string {
	sum := sha256.Sum256(evidence)
	return interactionID + "-" + hex.EncodeToString(sum[:16])
}

// markRejected records, create-new, that the hook could not verify this
// allow's evidence.
func markRejected(home, sessionID, interactionID string, evidence json.RawMessage, reason string) error {
	dir, err := ensureApproveSubdir(home, sessionID, "rejected")
	if err != nil {
		return err
	}
	err = createNewJSON(dir, proofName(interactionID, evidence), map[string]string{"reason": reason})
	if errors.Is(err, errFileExists) {
		return nil
	}
	return err
}

// rejectedRecord reads the rejected record of this allow's evidence.
func rejectedRecord(home, sessionID, interactionID string, evidence json.RawMessage) (reason string, ok bool) {
	path := filepath.Join(approveDir(home, sessionID), "rejected", proofName(interactionID, evidence))
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	var r struct {
		Reason string `json:"reason"`
	}
	_ = readApprovalJSON(path, &r)
	return r.Reason, true
}

// rejected reports whether the hook refused this allow's evidence.
func rejected(home, sessionID, interactionID string, evidence json.RawMessage) bool {
	v, ok := hookVerdict(home, sessionID, interactionID, evidence)
	return ok && v.Verdict == verdictRefused
}

// verdicts/<iid>-<proof> is the hook's one verdict on one allow proof,
// written create-new beside the rejected record (611.42.6, Pro review of
// #986 r1). The hook allows a proof only after it created the allow
// verdict and then found no rejected record for it. It records a refusal
// as a refused verdict and still as the rejected record, which an older
// endpoint reads. A reader of both reads the rejected record first and the
// verdict second, the reverse of the hook's order, so any refusal the
// endpoint acts on, forged or not, is one the hook sees before it allows.
type approvalVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Altered bool   `json:"altered,omitempty"`
}

const (
	verdictAllow   = "allow"
	verdictRefused = "refused"
)

// claimVerdict records v as the hook's verdict on this allow's evidence,
// create-new: errFileExists means a verdict stands already.
func claimVerdict(home, sessionID, interactionID string, evidence json.RawMessage, v approvalVerdict) error {
	dir, err := ensureApproveSubdir(home, sessionID, "verdicts")
	if err != nil {
		return err
	}
	return createNewJSON(dir, proofName(interactionID, evidence), v)
}

// hookVerdict reads the hook's verdict on this allow's evidence: its
// verdict record, or, from a hook that writes none, its rejected record as
// a refusal, altered when its reason says so. false means the hook has not
// decided.
func hookVerdict(home, sessionID, interactionID string, evidence json.RawMessage) (approvalVerdict, bool) {
	reason, refused := rejectedRecord(home, sessionID, interactionID, evidence)
	var v approvalVerdict
	err := readApprovalJSON(filepath.Join(approveDir(home, sessionID), "verdicts", proofName(interactionID, evidence)), &v)
	switch {
	case err == nil && (v.Verdict == verdictAllow || v.Verdict == verdictRefused):
		return v, true
	case refused:
		return approvalVerdict{Verdict: verdictRefused, Reason: reason, Altered: strings.Contains(reason, ErrAllowAltered.Error())}, true
	}
	return approvalVerdict{}, false
}

// ApprovalPin is the DM edge's pin file content. Owner is the share's owner
// pubkey: the attachment offers allow only when the installed hook pins the
// same owner. It decides the offer only; the hook's own pin decides allow.
type approvalPin struct {
	PID   int    `json:"pid"`
	Share string `json:"share"`
	Owner string `json:"owner,omitempty"`
}

// PinApprovals marks the session as served by a relay share that answers
// approvals from the DM: the PermissionRequest hook decides nothing without
// a pin whose pid is alive. It returns the pin's token for UnpinApprovals.
func PinApprovals(home, sessionID, share, owner string, pid int) (string, error) {
	dir, err := ensureApproveSubdir(home, sessionID, "")
	if err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	if err := createNewJSON(dir, "pin-"+token, approvalPin{PID: pid, Share: share, Owner: owner}); err != nil {
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
	return len(livePins(home, sessionID)) > 0
}

// livePins are the session's pins whose serving pid is alive.
func livePins(home, sessionID string) []approvalPin {
	dir := approveDir(home, sessionID)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []approvalPin
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), "pin-") {
			continue
		}
		var p approvalPin
		if readApprovalJSON(filepath.Join(dir, e.Name()), &p) != nil {
			continue
		}
		if alive, err := pidAlive(p.PID); err == nil && alive {
			out = append(out, p)
		}
	}
	return out
}

// allowPinned reports whether an allow can reach this session's calls: the
// installed hook pins a whole share, and a live share pin names the same
// owner and share session. It decides only whether the DM offers allow.
func allowPinned(home, sessionID string) bool {
	pin := PermissionHookPin(home)
	if !pin.Complete() {
		return false
	}
	for _, p := range livePins(home, sessionID) {
		if p.Owner == pin.Owner && p.Share == pin.Session {
			return true
		}
	}
	return false
}

// ownerHexRe is a pinned owner: a 64 lowercase hex x-only public key.
var ownerHexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidOwner reports whether owner is a 64 lowercase hex public key.
func ValidOwner(owner string) bool { return ownerHexRe.MatchString(owner) }

// AllowShare is the trusted identity of the relay share that serves one
// Claude session, read from the AMQ root the hook command line pins: the
// owner, the share's body pubkey, DM channel and target, and where the
// verifier reads the approval message's history.
type AllowShare struct {
	Owner, Body, Channel, Target string
	Session, RelayURL            string
}

// AllowCheck is what an allow must prove: the share, the hook's own
// rendering of the call, and the window the owner's reaction must be dated
// in. None of it comes from the answer file.
type AllowCheck struct {
	Share               AllowShare
	Prompt              string
	NotBefore, NotAfter time.Time
}

// AllowVerifier checks the evidence of an allow answer against want: the
// pinned owner's signed approve reaction on the share's approval message
// that shows exactly want.Prompt, and that message's history on the relay.
// A nil error is the only proof the hook accepts. An error that wraps
// ErrAllowAltered means the message changed after it was posted.
type AllowVerifier func(ctx context.Context, evidence json.RawMessage, want AllowCheck) error

// ErrAllowAltered is a verification that found the approval message
// altered after it was posted: an edit that shows another call, or a
// deletion.
var ErrAllowAltered = errors.New("the approval message was altered after it was posted")

// AllowConfig is what allow from Buzz needs: the pinned owner, the share
// that serves a session, and the verifier. Without all three, allow is
// impossible.
type AllowConfig struct {
	Owner  string
	Share  func(sessionID string) (AllowShare, error)
	Verify AllowVerifier
}

func (c AllowConfig) usable() bool { return ValidOwner(c.Owner) && c.Share != nil && c.Verify != nil }

// share resolves the session's share and requires it to name the pinned
// owner and a whole identity.
func (c AllowConfig) share(sessionID string) (AllowShare, error) {
	sh, err := c.Share(sessionID)
	switch {
	case err != nil:
		return sh, err
	case sh.Owner != c.Owner || !ValidOwner(sh.Body) || sh.Channel == "" || sh.Target == "":
		return sh, errors.New("the share does not name the pinned owner, its body, DM channel and target")
	}
	return sh, nil
}

// HookPin is what the PermissionRequest hook command line pins: the owner
// pubkey and the share an allow must come from, as install-approval-hook
// read it at install time: its relay URL, body pubkey, DM channel and
// target. Root and Session only locate the enrolled body secret, which must
// match Body. Claude cannot edit the settings file that holds the pin
// without a prompt, and no file outside the pin changes what an allow must
// prove.
type HookPin struct {
	Owner, Root, Session         string
	Relay, Body, Channel, Target string
}

// Complete reports whether the pin names everything an allow needs.
func (p HookPin) Complete() bool {
	return ValidOwner(p.Owner) && ValidOwner(p.Body) && filepath.IsAbs(p.Root) &&
		p.Session != "" && p.Relay != "" && p.Channel != "" && p.Target != ""
}

// allowFactory builds the AllowConfig for a pin; cmd registers the one
// that reads the manifest and the relay.
var allowFactory func(HookPin) AllowConfig

// RegisterAllowFactory sets how a pin becomes an AllowConfig, for the hook
// and for the attachment, which verifies an allow before it answers.
func RegisterAllowFactory(f func(HookPin) AllowConfig) { allowFactory = f }

// Owner-facing replies for an allow that did not verify.
const alteredReply = "The approval message was altered after it was posted; check the terminal."

func retryReply(reason string) string {
	return "Could not verify the approval: " + reason + ". React again to retry."
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
