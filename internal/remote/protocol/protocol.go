// Package protocol defines the AMQ Remote application protocol: the command
// a client sends to an amq-remote endpoint, the request snapshot the endpoint
// answers with, and the session projection it advertises. The JSON shapes are
// frozen in schemas/remote-*-v1.schema.json; this package is the Go reading of
// those schemas plus the strict decoder every carrier (local IPC, AMQ mailbox,
// Buzz DM edge) runs before any state changes.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema identifiers. A document with any other value is refused.
const (
	SchemaCommand = "amq.remote.command/1"
	SchemaRequest = "amq.remote.request/1"
	SchemaSession = "amq.remote.session/1"
)

// Reference limits from the design. Bounds are enforced before allocation of
// anything derived from the payload. The byte budgets are coherent by
// construction: a record always holds one max-size result plus the retained
// input and record overhead, so MaxRecordBytes >= MaxResultBytes + MaxInputBytes
// + headroom. Truncation is UTF-8-rune safe and never drops the native
// reference, so a truncated answer stays locatable on the target host.
const (
	MaxCommandBytes = 256 * 1024
	MaxInputBytes   = 128 * 1024
	MaxResultBytes  = 512 * 1024
	// MaxRecordBytes bounds one durable request record on disk and on the wire.
	// It is the protocol result bound plus the retained input bound plus fixed
	// headroom for the snapshot, interaction, cancel and bookkeeping fields, so
	// a record carrying a full-size result never overflows the store. The store
	// and the IPC layer share this single source of truth.
	MaxRecordOverhead = 64 * 1024
	MaxRecordBytes    = MaxResultBytes + MaxInputBytes + MaxRecordOverhead
	MaxOpaqueLen      = 128
	MaxOptionLen      = 256
)

// Op is the command discriminator.
type Op string

// Commands a client may send. The set is closed; view.open and view.close
// arrive with the terminal backend and are refused until then.
const (
	OpRequestSubmit      Op = "request.submit"
	OpRequestGet         Op = "request.get"
	OpRequestCancel      Op = "request.cancel"
	OpSessionList        Op = "session.list"
	OpSessionInspect     Op = "session.inspect"
	OpSessionEvents      Op = "session.events"
	OpInteractionRespond Op = "interaction.respond"
)

// Busy tells the target what to do when the runtime is already working.
type Busy string

// Busy policies. The default is reject; queue is an explicit opt-in.
const (
	BusyReject Busy = "reject"
	BusyQueue  Busy = "queue"
)

// Deliver selects the native delivery mode for a submit.
type Deliver string

// Delivery modes. turn starts or queues a new top-level turn; steer
// interleaves into the running turn where the harness supports it.
const (
	DeliverTurn  Deliver = "turn"
	DeliverSteer Deliver = "steer"
)

// SubmitInput is the work carried by request.submit.
type SubmitInput struct {
	Text    string  `json:"text"`
	Busy    Busy    `json:"busy,omitempty"`
	Deliver Deliver `json:"deliver,omitempty"`
}

// Command is the decoded client command. Fields not used by an op are empty.
type Command struct {
	Schema        string       `json:"schema"`
	Op            Op           `json:"op"`
	RequestID     string       `json:"request_id,omitempty"`
	RequestRef    string       `json:"request_ref,omitempty"`
	TargetID      string       `json:"target_id,omitempty"`
	Epoch         string       `json:"epoch,omitempty"`
	NotAfter      string       `json:"not_after,omitempty"`
	Input         *SubmitInput `json:"input,omitempty"`
	Since         *int64       `json:"since,omitempty"`
	InteractionID string       `json:"interaction_id,omitempty"`
	Option        string       `json:"option,omitempty"`
}

// State is the durable request state.
type State string

// Request states. See the remote-control ADR for their exact meaning.
const (
	StateReceived    State = "received"
	StateDispatching State = "dispatching"
	StateRunning     State = "running"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCancelled   State = "cancelled"
	StateRejected    State = "rejected"
	StateUncertain   State = "uncertain"
)

// Terminal reports whether no further native evidence can change the state.
// uncertain is not terminal: later exact evidence may resolve it.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateRejected:
		return true
	}
	return false
}

// Code is the typed reason attached to rejected, failed, cancelled and
// uncertain records, and to command refusals.
type Code string

// Typed codes. The set is closed so clients can switch on it.
const (
	CodeBusy                     Code = "busy"
	CodeExpired                  Code = "expired"
	CodeStaleEpoch               Code = "stale_epoch"
	CodeUnsupported              Code = "unsupported"
	CodeUnshared                 Code = "unshared"
	CodeInvalid                  Code = "invalid"
	CodeRequestConflict          Code = "request_conflict"
	CodeCancelledBeforeAdmission Code = "cancelled_before_admission"
	CodeCancelledByRequest       Code = "cancelled_by_request"
	CodeNativeError              Code = "native_error"
	CodeStorageFull              Code = "storage_full"
	CodeAttachmentLost           Code = "attachment_lost"
	CodeResultExpired            Code = "result_expired"
	CodeNotFound                 Code = "not_found"
	CodeAlreadyResolved          Code = "already_resolved"
	CodeEndpointAlreadyRunning   Code = "endpoint_already_running"
	CodeEndpointUnreachable      Code = "endpoint_unreachable"
	CodeStoreClosed              Code = "store_closed"
)

// CancelDisposition is the recorded outcome of a cancel command.
type CancelDisposition string

// Cancel dispositions. cancel_requested stays until native confirmation.
const (
	CancelRequested    CancelDisposition = "cancel_requested"
	CancelConfirmed    CancelDisposition = "cancelled"
	CancelNoopTerminal CancelDisposition = "noop_already_terminal"
	CancelUnsupported  CancelDisposition = "unsupported"
)

// Cancel records cancellation intent and its disposition.
type Cancel struct {
	RequestedAt string            `json:"requested_at,omitempty"`
	Disposition CancelDisposition `json:"disposition"`
}

// Interaction is a pending native question or approval.
type Interaction struct {
	InteractionID string   `json:"interaction_id"`
	Kind          string   `json:"kind"`
	Prompt        string   `json:"prompt,omitempty"`
	Options       []string `json:"options"`
	RemoteAnswer  bool     `json:"remote_answer,omitempty"`
}

// Result is the bounded native outcome kept for remote delivery.
type Result struct {
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	StopReason string `json:"stop_reason,omitempty"`
	Error      string `json:"error,omitempty"`
	NativeRef  string `json:"native_ref,omitempty"`
}

// Snapshot is the durable request record and the reply shape for every
// request.* command. Each published revision is immutable.
type Snapshot struct {
	Schema            string       `json:"schema"`
	RequestRef        string       `json:"request_ref"`
	RequestID         string       `json:"request_id"`
	CreatorHost       string       `json:"creator_host"`
	TargetID          string       `json:"target_id"`
	Epoch             string       `json:"epoch"`
	Revision          int64        `json:"revision"`
	State             State        `json:"state"`
	Code              Code         `json:"code,omitempty"`
	InputDigest       string       `json:"input_digest,omitempty"`
	NotAfter          string       `json:"not_after,omitempty"`
	NativeRun         *string      `json:"native_run"`
	LocalIntervention bool         `json:"local_intervention,omitempty"`
	Cancel            *Cancel      `json:"cancel,omitempty"`
	Interaction       *Interaction `json:"interaction,omitempty"`
	Result            *Result      `json:"result,omitempty"`
	ObservedAt        string       `json:"observed_at"`
}

// Outcome is the per-COMMAND result, distinct from the immutable per-revision
// Snapshot. It carries the op-specific disposition that does NOT create a new
// revision: a request_conflict, an already-resolved interaction, or a cancel
// that hit a terminal record. Pairing the unchanged snapshot with an Outcome
// (instead of copy-mutating the snapshot's Code/Cancel) keeps the invariant
// that equal revisions are byte-identical, so retries recover the original
// command rather than regenerating a different one.
type Outcome struct {
	Op          Op                `json:"op"`
	Code        Code              `json:"code,omitempty"`        // request_conflict, already_resolved, "" on plain success
	Message     string            `json:"message,omitempty"`
	Disposition CancelDisposition `json:"disposition,omitempty"` // cancel replies only
}

// Reply pairs the current immutable record revision with the operation
// outcome. The Snapshot is byte-identical to a stored revision and is NEVER
// mutated for op-specific reasons; every op-specific signal lives in Outcome.
// Carriers and the CLI map exit codes from Outcome.Code.
type Reply struct {
	Snapshot Snapshot `json:"snapshot"`
	Outcome  Outcome  `json:"outcome"`
}

// Capabilities is the observed capability projection of one attachment.
type Capabilities struct {
	Inspect        bool   `json:"inspect"`
	Submit         bool   `json:"submit"`
	CancelRequest  bool   `json:"cancel_request"`
	AnswerQuestion bool   `json:"answer_question"`
	ApproveTool    bool   `json:"approve_tool"`
	Steer          bool   `json:"steer"`
	Terminal       string `json:"terminal"`
}

// Evidence names the strongest evidence class an attachment gives.
type Evidence struct {
	Submit     string `json:"submit,omitempty"`
	Completion string `json:"completion,omitempty"`
}

// Session is the projection of one registered runtime.
type Session struct {
	Schema             string       `json:"schema"`
	TargetID           string       `json:"target_id"`
	Epoch              string       `json:"epoch"`
	Harness            string       `json:"harness"`
	DisplayName        string       `json:"display_name,omitempty"`
	Host               string       `json:"host,omitempty"`
	Project            string       `json:"project,omitempty"`
	Attachment         string       `json:"attachment"`
	Status             string       `json:"status"`
	PendingInteraction *string      `json:"pending_interaction,omitempty"`
	ActiveRequestRef   *string      `json:"active_request_ref,omitempty"`
	Capabilities       Capabilities `json:"capabilities"`
	Evidence           *Evidence    `json:"evidence,omitempty"`
	ObservedAt         string       `json:"observed_at"`
}

// Refusal is a typed protocol-level refusal. It carries the code a client can
// switch on and maps to an AMQ exit code through ExitCode.
type Refusal struct {
	Code    Code
	Message string
}

func (r *Refusal) Error() string {
	if r.Message == "" {
		return string(r.Code)
	}
	return fmt.Sprintf("%s: %s", r.Code, r.Message)
}

// Refuse builds a Refusal.
func Refuse(code Code, format string, args ...any) error {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Exit codes follow the AMQ contract; see internal/cli/exitcode.go.
const (
	ExitSuccess        = 0
	ExitError          = 1
	ExitUsage          = 2
	ExitNotFound       = 3
	ExitTimeout        = 4
	ExitActionRequired = 6
	ExitInterrupted    = 130
)

// ExitCode maps a command error to the AMQ exit code contract. nil is
// success. Refusals map by code; any other error is a general error.
func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	return ExitForCode(RefusalCode(err))
}

// RefusalCode extracts the Code from a Refusal error, or "" if the error is not
// a Refusal. It lets callers branch on the refusal code without repeating the
// errors.As dance.
func RefusalCode(err error) Code {
	if err == nil {
		return ""
	}
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// ExitForCode maps a refusal code to the AMQ exit code contract.
func ExitForCode(code Code) int {
	switch code {
	case CodeInvalid:
		return ExitUsage
	case CodeNotFound:
		return ExitNotFound
	case CodeBusy, CodeUnsupported, CodeUnshared, CodeExpired, CodeStaleEpoch,
		CodeRequestConflict, CodeStorageFull, CodeAttachmentLost, CodeResultExpired,
		CodeAlreadyResolved, CodeEndpointAlreadyRunning, CodeEndpointUnreachable,
		CodeStoreClosed:
		return ExitActionRequired
	}
	return ExitError
}

// ExitForState maps a terminal-or-not snapshot to the exit code `wait` and
// `status` report. A non-terminal state during a bounded wait is a timeout.
func ExitForState(s State) int {
	switch s {
	case StateCompleted:
		return ExitSuccess
	case StateFailed, StateCancelled:
		return ExitError
	case StateRejected, StateUncertain:
		return ExitActionRequired
	}
	return ExitTimeout
}

// uuidLen is the fixed length of a lowercase UUID request_id.
const uuidLen = 36

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	opaqueRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

// RefPrefix starts every request reference.
const RefPrefix = "amqr1_"

// MaxRefLen is the maximum length of a request reference. It is derived from
// the opaque-segment bound (creator_host and target_id, each MaxOpaqueLen) plus
// the fixed UUID request_id and the two NUL separators, base32-encoded without
// padding (8 chars per 5 bytes, rounded up), plus the RefPrefix. The regex and
// the schemas use this as the upper bound so a receipt for a max-size key
// always round-trips (BK6: the previous 200-char cap rejected valid max-size
// refs of up to 477 chars).
const MaxRefLen = len(RefPrefix) + (MaxOpaqueLen*2+uuidLen+2+4)/5*8

var refRe = regexp.MustCompile(`^amqr1_[a-z2-7]{16,` + strconv.Itoa(MaxRefLen) + `}$`)

var refEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// EncodeRef builds the opaque request reference for a record. Clients copy
// it from a receipt; only the endpoint constructs it.
func EncodeRef(creatorHost, targetID, requestID string) string {
	raw := strings.Join([]string{creatorHost, targetID, requestID}, "\x00")
	return RefPrefix + strings.ToLower(refEncoding.EncodeToString([]byte(raw)))
}

// DecodeRef recovers the record key from a reference. It identifies a record;
// it never changes the peer's authority or selects a filesystem path.
func DecodeRef(ref string) (creatorHost, targetID, requestID string, err error) {
	if !refRe.MatchString(ref) {
		return "", "", "", Refuse(CodeInvalid, "malformed request_ref")
	}
	raw, err := refEncoding.DecodeString(strings.ToUpper(strings.TrimPrefix(ref, RefPrefix)))
	if err != nil {
		return "", "", "", Refuse(CodeInvalid, "undecodable request_ref")
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 || !validOpaque(parts[0]) || !validOpaque(parts[1]) || !uuidRe.MatchString(parts[2]) {
		return "", "", "", Refuse(CodeInvalid, "request_ref does not name a record")
	}
	return parts[0], parts[1], parts[2], nil
}

func validOpaque(s string) bool {
	return s != "" && len(s) <= MaxOpaqueLen && opaqueRe.MatchString(s)
}

// digestPrefix is the algorithm tag every request input_digest carries.
const digestPrefix = "sha256:"

// CommandDigest is the digest of the immutable submit command payload, over
// exactly {schema, op, request_id, target_id, epoch, not_after, input}. It is
// canonical JSON: object keys sorted, no insignificant whitespace, so every
// carrier agrees on the bytes. A retry with a changed epoch or not_after (or
// input) yields a different digest and is request_conflict; a retry that
// recovers the original command bytes yields the same digest. The digest
// excludes revision/state/result — those are server-derived, not part of the
// client's command.
//
// Canonical byte construction (for carriers that build the bytes themselves):
//   json.Marshal of digestPayload{Schema,Op,RequestID,TargetID,Epoch,NotAfter,Input}
//   with struct field order fixed (Go json emits in struct order, which is the
//   canonical order below) and no extra whitespace, then sha256 hex with the
//   "sha256:" prefix.
func CommandDigest(cmd *Command) string {
	if cmd == nil || cmd.Op != OpRequestSubmit {
		return ""
	}
	payload := digestPayload{
		Schema:    cmd.Schema,
		Op:        cmd.Op,
		RequestID: cmd.RequestID,
		TargetID:  cmd.TargetID,
		Epoch:     cmd.Epoch,
		NotAfter:  cmd.NotAfter,
		Input:     cmd.Input,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return digestPrefix + hex.EncodeToString(sum[:])
}

// digestPayload is the stable canonical shape for CommandDigest. Field order
// is the canonical key order; do not reorder without bumping the schema and
// migrating records. omitempty is intentionally absent on submit-required
// fields so the canonical shape is identical for every valid submit.
type digestPayload struct {
	Schema    string       `json:"schema"`
	Op        Op           `json:"op"`
	RequestID string       `json:"request_id"`
	TargetID  string       `json:"target_id"`
	Epoch     string       `json:"epoch"`
	NotAfter  string       `json:"not_after"`
	Input     *SubmitInput `json:"input"`
}

// DecodeCommand strictly decodes one command document. It enforces the size
// bound before decoding, rejects unknown and duplicate keys, and validates
// every field the op requires. The returned Command is safe to act on.
func DecodeCommand(data []byte) (*Command, error) {
	if len(data) > MaxCommandBytes {
		return nil, Refuse(CodeInvalid, "command exceeds %d bytes", MaxCommandBytes)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Command
	if err := dec.Decode(&c); err != nil {
		return nil, Refuse(CodeInvalid, "malformed command: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, Refuse(CodeInvalid, "trailing data after command")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the op-specific contract. It is exported so carriers that
// build a Command in memory (the Buzz DM edge, the CLI) share one rule set.
func (c *Command) Validate() error {
	if c.Schema != SchemaCommand {
		return Refuse(CodeInvalid, "schema must be %s", SchemaCommand)
	}
	switch c.Op {
	case OpRequestSubmit:
		if err := requireFields(c, "request_id", "target_id", "epoch", "not_after", "input"); err != nil {
			return err
		}
		if err := forbidFields(c, "request_ref", "since", "interaction_id", "option"); err != nil {
			return err
		}
		if c.Input.Text == "" || len(c.Input.Text) > MaxInputBytes {
			return Refuse(CodeInvalid, "input.text must be 1..%d bytes", MaxInputBytes)
		}
		switch c.Input.Busy {
		case "", BusyReject, BusyQueue:
		default:
			return Refuse(CodeInvalid, "input.busy must be reject or queue")
		}
		switch c.Input.Deliver {
		case "", DeliverTurn, DeliverSteer:
		default:
			return Refuse(CodeInvalid, "input.deliver must be turn or steer")
		}
	case OpRequestGet:
		if err := requireFields(c, "request_ref"); err != nil {
			return err
		}
		if err := forbidFields(c, "request_id", "target_id", "epoch", "not_after", "input", "since", "interaction_id", "option"); err != nil {
			return err
		}
	case OpRequestCancel:
		if err := requireFields(c, "request_ref", "target_id", "epoch", "not_after"); err != nil {
			return err
		}
		if err := forbidFields(c, "request_id", "input", "since", "interaction_id", "option"); err != nil {
			return err
		}
	case OpSessionList:
		if err := forbidFields(c, "request_id", "request_ref", "target_id", "epoch", "not_after", "input", "since", "interaction_id", "option"); err != nil {
			return err
		}
	case OpSessionInspect:
		if err := requireFields(c, "target_id"); err != nil {
			return err
		}
		if err := forbidFields(c, "request_id", "request_ref", "epoch", "not_after", "input", "since", "interaction_id", "option"); err != nil {
			return err
		}
	case OpSessionEvents:
		if err := requireFields(c, "target_id"); err != nil {
			return err
		}
		if err := forbidFields(c, "request_id", "request_ref", "epoch", "not_after", "input", "interaction_id", "option"); err != nil {
			return err
		}
		if c.Since != nil && *c.Since < 0 {
			return Refuse(CodeInvalid, "since must be >= 0")
		}
	case OpInteractionRespond:
		if err := requireFields(c, "request_ref", "target_id", "epoch", "interaction_id", "option"); err != nil {
			return err
		}
		if err := forbidFields(c, "request_id", "not_after", "input", "since"); err != nil {
			return err
		}
		if len(c.Option) > MaxOptionLen {
			return Refuse(CodeInvalid, "option exceeds %d bytes", MaxOptionLen)
		}
	default:
		return Refuse(CodeInvalid, "unknown op %q", string(c.Op))
	}
	return nil
}

func requireFields(c *Command, names ...string) error {
	for _, n := range names {
		switch n {
		case "request_id":
			if !uuidRe.MatchString(c.RequestID) {
				return Refuse(CodeInvalid, "request_id must be a lowercase UUID")
			}
		case "request_ref":
			if _, _, _, err := DecodeRef(c.RequestRef); err != nil {
				return err
			}
		case "target_id":
			if !validOpaque(c.TargetID) {
				return Refuse(CodeInvalid, "target_id is required and opaque")
			}
		case "epoch":
			if !validOpaque(c.Epoch) {
				return Refuse(CodeInvalid, "epoch is required and opaque")
			}
		case "interaction_id":
			if !validOpaque(c.InteractionID) {
				return Refuse(CodeInvalid, "interaction_id is required and opaque")
			}
		case "option":
			if c.Option == "" {
				return Refuse(CodeInvalid, "option is required")
			}
		case "not_after":
			if _, err := ParseTime(c.NotAfter); err != nil {
				return Refuse(CodeInvalid, "not_after must be RFC 3339")
			}
		case "input":
			if c.Input == nil {
				return Refuse(CodeInvalid, "input is required")
			}
		}
	}
	return nil
}

func forbidFields(c *Command, names ...string) error {
	for _, n := range names {
		present := false
		switch n {
		case "request_id":
			present = c.RequestID != ""
		case "request_ref":
			present = c.RequestRef != ""
		case "target_id":
			present = c.TargetID != ""
		case "epoch":
			present = c.Epoch != ""
		case "not_after":
			present = c.NotAfter != ""
		case "input":
			present = c.Input != nil
		case "since":
			present = c.Since != nil
		case "interaction_id":
			present = c.InteractionID != ""
		case "option":
			present = c.Option != ""
		}
		if present {
			return Refuse(CodeInvalid, "%s is not allowed for %s", n, string(c.Op))
		}
	}
	return nil
}

// ParseTime parses an RFC 3339 timestamp as the protocol requires.
func ParseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// TruncateText bounds text to at most max bytes on a UTF-8 rune boundary, so
// no multi-byte rune is split. It reports whether truncation happened. A
// truncated result must keep its native reference so the full text stays
// locatable on the target host; callers set Result.Truncated and keep
// Result.NativeRef rather than dropping them.
func TruncateText(text string, max int) (string, bool) {
	if len(text) <= max {
		return text, false
	}
	// Step back to the last rune start at or before max bytes.
	end := max
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	// If the byte at end is not itself a valid rune start, back up until it is.
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}

// FormatTime renders a timestamp in the one form the protocol emits.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// rejectDuplicateKeys walks the token stream and refuses any object with a
// repeated key. encoding/json keeps the last value silently, which would let
// two carriers disagree about the same bytes.
func rejectDuplicateKeys(data []byte) error {
	type frame struct {
		object    bool
		keys      map[string]struct{}
		expectKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return Refuse(CodeInvalid, "malformed command: %v", err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]struct{}{}, expectKey: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = true
				}
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		top := stack[len(stack)-1]
		if !top.object {
			continue
		}
		if top.expectKey {
			key, _ := tok.(string)
			if _, dup := top.keys[key]; dup {
				return Refuse(CodeInvalid, "duplicate key %q", key)
			}
			top.keys[key] = struct{}{}
			top.expectKey = false
			continue
		}
		top.expectKey = true
	}
}
