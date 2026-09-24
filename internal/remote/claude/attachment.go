// Package claude is the Claude Code adapter factory (bead 611.12, PR1).
// It registers the kind:"claude" factory so a manifest entry
// `{"kind":"claude","target":"<name>","config":{...}}` builds a
// claudeAttachment without editing serve.
//
// PR1 scope (architect ruling 10:59Z, recut per review-852-r1): the
// adapter skeleton, file-only inspect (session registry + transcript
// tail — NO child process is ever spawned from Inspect), and an HONEST
// capability projection — submit is advertised false and refusal-only
// until PR2 wires the pinned 611.2 wire
// (docs/research/r0-03-cc-socket-wire-capture.md, merged at 17bff18).
// No keystrokes, no print child, no `claude -p` fallback, no process
// ownership (ADR invariant 1). The Stop-hook installer is DEFERRED to
// PR2 together with its receiver subcommand (review-852-r1 P1-3: an
// installed hook whose command does not exist would exit 2, Claude
// Code's blocking-error code — the opposite of failing open).
//
// Evidence vocabulary (architect ruling 10:59Z; "delivered" does not
// exist in the schema — #822 removed it): a socket ack alone is
// TENTATIVE (bound, ownership not proven); the transcript user line is
// SUBMITTED; the assistant turn starting (Stop-hook post or transcript
// assistant line) is ADMITTED. PR1 issues none of these over a wire:
// submit refuses before any side effect.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// SentinelUnpinned matches the other seam-backed adapters: until PR2 binds
// a run, no epoch is ever pinned by this adapter.
const SentinelUnpinned = "unpinned"

// config is the adapter-specific config block for a claude manifest entry.
type config struct {
	// Pid is the target Claude Code process id. The adapter resolves the
	// session registry file (~/.claude/sessions/<pid>.json) from it; the
	// pid also keys the transcript under ~/.claude/projects via sessionId.
	Pid int `json:"pid"`
	// Target overrides the published target id (the manifest's declared
	// identity, the same rule as codex/amit). Empty = the registry name,
	// validated, else "claude:<pid>".
	Target string `json:"target,omitempty"`
	// Home overrides the Claude home for tests; empty = os.UserHomeDir().
	Home string `json:"home,omitempty"`
}

// sessionRegistry is the subset of ~/.claude/sessions/<pid>.json the
// adapter reads. Unknown fields are ignored (forward-compatible).
type sessionRegistry struct {
	Pid                 int      `json:"pid"`
	SessionID           string   `json:"sessionId"`
	Cwd                 string   `json:"cwd"`
	Version             string   `json:"version"`
	PeerProtocol        int      `json:"peerProtocol"`
	PeerFeatures        []string `json:"peerFeatures"`
	Kind                string   `json:"kind"`
	MessagingSocketPath string   `json:"messagingSocketPath"`
	Name                string   `json:"name"`
	Status              string   `json:"status"`
	UpdatedAt           int64    `json:"updatedAt"` // unix millis
}

// maxRegistryBytes bounds the session-registry read: a real entry is well
// under 1 KiB; anything larger on a local-process-writable path is refused
// before it can stall a read under the endpoint mutex (r2 P1-1).
const maxRegistryBytes = 64 << 10

// Factory builds a claude Attachment from a registry.FactoryConfig.
func Factory(_ context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("parse claude config: %w", err)
		}
	}
	if c.Pid <= 0 {
		return nil, fmt.Errorf("claude config: pid is required (the target Claude Code process id)")
	}
	// The manifest target is the identity the endpoint addresses this
	// adapter under (codex factory comment, ".13"); cfg.Target wins over
	// any registry-derived name, which crosses a trust boundary (the
	// registry file is local-process-writable).
	if cfg.Target != "" {
		c.Target = cfg.Target
	}
	att, err := Attach(c)
	if err != nil {
		return nil, err
	}
	return att, nil
}

// Attach builds the attachment, resolving the session registry once. A
// missing registry entry is a startup refusal: the adapter never guesses a
// session identity (contract §Identity posture shared with the amit
// adapter).
func Attach(cfg config) (*Attachment, error) {
	home := cfg.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("claude adapter: resolve home: %w", err)
		}
		home = h
	}
	reg, err := readSessionRegistry(home, cfg.Pid)
	if err != nil {
		return nil, fmt.Errorf("claude adapter: %w", err)
	}
	if reg == nil {
		// No registry entry for the pid: a claude session we cannot identify
		// is never attached (a missing registry file is "unknown session",
		// not "no registry kind") (r3: readSessionRegistry now returns
		// nil,nil on absence; Attach refuses it).
		return nil, fmt.Errorf("claude adapter: pid %d has no session registry entry; refusing", cfg.Pid)
	}
	if reg.Kind != "" && reg.Kind != "interactive" {
		return nil, fmt.Errorf("claude adapter: pid %d is kind %q; only interactive sessions attach", cfg.Pid, reg.Kind)
	}
	target := cfg.Target
	if target == "" {
		target = deriveTarget(cfg.Pid, reg)
	}
	if !protocol.ValidTargetID(target) {
		return nil, fmt.Errorf("claude adapter: derived target id %q is not protocol-addressable (opaque grammar); set an explicit target in the manifest", target)
	}
	att := &Attachment{
		target:       target,
		cfg:          cfg,
		home:         home,
		boundSession: reg.SessionID,
		runs:         map[requests.Key]*runRecord{},
		cancelIntent: map[requests.Key]bool{},
		recoverFrom:  map[requests.Key]recoverScan{},
		released:     map[requests.Key]struct{}{},
		ctx:          context.Background(),
	}
	token, err := bindStopSession(home, reg.SessionID)
	if err != nil && !errors.Is(err, errUnsupportedPlatform) {
		return nil, fmt.Errorf("claude adapter: %w", err)
	}
	att.boundToken = token
	return att, nil
}

// deriveTarget prefers the registry name when it is protocol-valid; the
// pid form is the fallback. The caller validates the result.
func deriveTarget(pid int, reg *sessionRegistry) string {
	if reg != nil && reg.Name != "" && protocol.ValidTargetID(reg.Name) {
		return reg.Name
	}
	return "claude:" + strconv.Itoa(pid)
}

func claudeSessionsDir(home string) string {
	return filepath.Join(home, ".claude", "sessions")
}

// readSessionRegistry reads ~/.claude/sessions/<pid>.json.
//
// Trust boundary (r3 P2-1): this is a Claude-internal registry file on a
// local-process-writable path, not an AMQ state leaf — so it does NOT get
// the state-leaf lstat refusal. Instead the read goes through
// readRegularBounded (safeopen.go): lstat gate, size bound before open,
// no-follow non-blocking open, fstat recheck, and a LimitReader, so neither
// a FIFO nor a growing file can stall or bloat a read under the endpoint
// mutex (r2 P1-1, r3 P1).
func readSessionRegistry(home string, pid int) (*sessionRegistry, error) {
	path := filepath.Join(claudeSessionsDir(home), strconv.Itoa(pid)+".json")
	raw, err := readRegularBounded(path, maxRegistryBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no registry entry yet: not an error
		}
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	var reg sessionRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	if reg.SessionID == "" {
		return nil, fmt.Errorf("session registry %s: sessionId is empty", path)
	}
	return &reg, nil
}

// Attachment implements core.Attachment over the Claude Code surfaces.
// It never injects keystrokes, never spawns a print child, and owns no
// process (ADR invariant 1). Inspect performs NO child-process spawn: it
// reads the session registry file only (review-852-r1 P0-1 — a spawn
// under the endpoint mutex froze every target for the life of a hung
// child; the roster poller moves to PR2 with the socket client, on a
// ticker into a cached value).
type Attachment struct {
	target string
	cfg    config
	home   string

	// PR2 evidence path (submit.go): retained runs, cancel intent, the
	// native-event sink, and the confirmation poller's cancel. ctx is the
	// attachment lifetime; the poller is never on the request path.
	mu            sync.Mutex
	runs          map[requests.Key]*runRecord
	cancelIntent  map[requests.Key]bool
	eventSink     func(core.NativeEvent)
	confirmCancel func()
	// stopConsumed is the byte offset in the session's Stop-marker file up
	// to which marker lines are settled (matched to a run or dropped as
	// orphans). Lines past it are preserved for the next poll.
	stopConsumed int64
	// boundSession is the session id published for the Stop-hook receiver.
	// Empty after unsubscribe. User off does not clear it. The receiver
	// writes for this id when no binding file is present, or when the
	// binding names this session.
	boundSession string
	// boundToken is this attachment's sentinel file under amq-bound.
	boundToken string
	// cur is the poller's transcript position; owner is the run that owns
	// the turn the cursor is inside, nil for a foreign or unknown turn.
	cur   transcriptCursor
	owner *runRecord
	// activitySink is the parsed-transcript observer. It is not the core
	// event sink, and its lifetime does not replace Subscribe.
	activitySink func(ActivityNote)
	activityTurn string
	// activityTurnTS is the transcript time of the user boundary that
	// opened activityTurn. An older Stop must not clear a later turn.
	activityTurnTS int64
	// activityPath and activityNext are how far activity has delivered.
	// Confirmation recovery may rewind cur; it must not replay these notes.
	// A truncated file resets activityNext. A recovery rewind does not.
	activityPath string
	activityNext int64
	// curGen increments whenever the cursor is reset outside the poller (a
	// recovered run needs a replay from its delivery offset); a poll that
	// started under an older generation does not write its cursor back.
	curGen uint64
	// recoverFrom[key] is how far a restart-recovery scan has read for a
	// key it has not found yet, so an uncertain record costs only the new
	// transcript bytes on each reconcile tick (611.25).
	recoverFrom map[requests.Key]recoverScan
	// released holds keys whose result the endpoint acknowledged: their
	// delivery is still in the transcript, and recovery must never bring
	// them back as a phantom running run. Bounded by maxReleased.
	released map[requests.Key]struct{}
	// closed: the endpoint unsubscribed. The poller is stopped and never
	// restarted; the attachment is being replaced or shut down.
	closed bool
	ctx    context.Context
}

// Inspect implements core.Attachment: the honest projection. Status is
// the registry file's own status field, normalized to the projection
// vocabulary {idle,busy,unknown,offline} (P2-1, mirroring codex
// threadStatus); the pid is liveness-checked (P2-4 — a stale registry
// left by a crashed session must not read "live" forever). PR2: submit
// is advertised TRUE — the pinned 611.2 socket wire is wired (submit.go);
// the interrupt-family seams remain false (no keystroke seam,
// docs/remote-compat.md §3). Evidence is nil on the projection: per-run
// evidence is carried by Lookup, and the projection fails closed under
// any caller floor.
func (a *Attachment) Inspect() protocol.Session {
	status, _ := a.observe()
	att := "live"
	if status == "offline" {
		att = "offline"
	}
	return protocol.Session{
		Schema:      protocol.SchemaSession,
		TargetID:    a.target,
		Epoch:       SentinelUnpinned,
		Harness:     "claude_code",
		DisplayName: "claude " + a.target,
		Attachment:  att,
		Status:      status,
		// PR2: submit true over the pinned socket wire; the rest stay
		// false — no interrupt seam without keystrokes.
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: false,
			ApproveTool: false, AnswerQuestion: false, Steer: false,
			Terminal: "unavailable",
		},
		Evidence:   nil, // per-run evidence is Lookup's answer; fails closed under a floor
		ObservedAt: protocol.FormatTime(a.now()),
	}
}

// normalizeStatus maps the registry's status vocabulary onto the
// projection's {idle,busy,unknown,offline}. Unknown values — including a
// hostile or future-vocabulary value from the local-process-writable
// registry — map to "unknown", never published raw (P2-1).
func normalizeStatus(s string) string {
	switch s {
	case "idle":
		return "idle"
	case "busy", "shell", "tool":
		return "busy"
	}
	return "unknown"
}

// NativeSessionID is the live registry entry's sessionId, the identity
// relay sharing pins (core.NativeIdentifier); empty when the session is not
// live.
func (a *Attachment) NativeSessionID() string {
	_, sessionID := a.observe()
	return sessionID
}

// observe returns the projection status and, for a live registry entry, its
// sessionId: the native identity relay sharing pins. It is a pure
// filesystem read: the registry file's status field plus a pid liveness
// check. No child process, no bound that can silently exceed its timeout
// (P0-1).
func (a *Attachment) observe() (string, string) {
	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil {
		return "offline", ""
	}
	if reg == nil {
		// No registry entry (r3: missing-file path) — offline.
		return "offline", ""
	}
	if alive, err := pidAlive(a.cfg.Pid); err != nil || !alive {
		return "offline", ""
	}
	// A registry status the process does not update any more is still
	// "unknown" in the projection, never a guessed idle.
	if reg.Status == "" {
		return "unknown", reg.SessionID
	}
	return normalizeStatus(reg.Status), reg.SessionID
}

func (a *Attachment) now() time.Time { return time.Now() }

// trackBoundSession publishes sessionID for the Stop-hook receiver when the
// live registry session changes. File IO stays under a.mu: the files are
// tiny and the unsubscribe path clears the same field under that lock.
func (a *Attachment) trackBoundSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.boundSession == sessionID {
		return
	}
	prev, prevToken := a.boundSession, a.boundToken
	a.boundSession = sessionID
	token, _ := bindStopSession(a.home, sessionID)
	a.boundToken = token
	if prev != "" {
		unbindStopSession(a.home, prev, prevToken)
	}
}

// Submit implements core.Attachment. PR1 refuses before ANY side effect:
// the endpoint gates on Capabilities.Submit=false before dispatch reaches
// the adapter (internal/remote/core/endpoint.go), so this is the second,
// adapter-side gate — fail closed twice rather than once.
// PR2: implemented in submit.go over the pinned 611.2 wire.

// Lookup is implemented in submit.go over the retained run map.

// CancelExact implements core.Attachment: typed refusal, never a zero
// value (P2-3 — an empty Disposition would persist as a fake outcome).
// Claude Code has no user-level interrupt without keystrokes (pinned
// evidence, docs/remote-compat.md §3), so cancellation is unsupported
// even in PR2.
func (a *Attachment) CancelExact(key requests.Key, _ string) (core.CancelEvidence, error) {
	return core.CancelEvidence{
		Disposition: protocol.CancelUnsupported,
		Message:     "claude_code has no interrupt seam without keystrokes (docs/remote-compat.md §3); cancellation is unsupported",
	}, nil
}

// Respond implements core.Attachment: no approval/question seam exists
// (docs/remote-compat.md §3; Remote Control is policy-disabled). The
// typed already_resolved code (P2-3 — an empty code reads as success at
// the endpoint) mirrors amit.
func (a *Attachment) Respond(_ requests.Key, _, _, _ string) (protocol.Code, error) {
	return protocol.CodeAlreadyResolved, nil
}

// AcknowledgeResult implements core.Attachment: nothing is retained, so
// the release is a no-op.
// AcknowledgeResult releases the retained terminal evidence for the key
// (mirrors codex): once the endpoint has persisted the outcome the
// adapter drops its record, so a restarted attachment's Unknown is honest.
func (a *Attachment) AcknowledgeResult(key requests.Key, _, _ string) {
	a.mu.Lock()
	delete(a.runs, key)
	delete(a.recoverFrom, key)
	if len(a.released) >= maxReleased {
		for k := range a.released { // drop an arbitrary old entry
			delete(a.released, k)
			break
		}
	}
	a.released[key] = struct{}{}
	a.mu.Unlock()
}

// maxReleased bounds the released-key set; the endpoint acknowledges each
// key once and never looks a released key up again in normal operation.
const maxReleased = 4096

// Subscribe registers the native-event sink (the confirmation poller
// emits status events; the Stop-hook receiver posts terminal events
// through the same sink). Returns the unsubscribe function.
//
// Unsubscribe is the attachment's end of life: the endpoint calls it when
// the target is replaced, unregistered or shut down, so it also stops the
// confirmation poller for good (codex #855 r2 item 6).
func (a *Attachment) Subscribe(cb func(core.NativeEvent)) func() {
	a.mu.Lock()
	a.eventSink = cb
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.eventSink = nil
		a.closed = true
		sid, token := a.boundSession, a.boundToken
		a.boundSession = ""
		a.boundToken = ""
		stop := a.confirmCancel
		a.confirmCancel = nil
		a.mu.Unlock()
		if sid != "" {
			unbindStopSession(a.home, sid, token)
		}
		if stop != nil {
			stop()
		}
	}
}

// ObserveActivity registers a parsed-transcript observer. It uses the
// confirmation poller already running for this attachment; it does not
// replace Subscribe or start a second reader. The poller keeps running
// while the observer is registered, including when no remote run is open.
// Unsubscribe releases that hold. The callback is not invoked with a.mu held.
func (a *Attachment) ObserveActivity(cb func(ActivityNote)) func() {
	a.mu.Lock()
	a.activitySink = cb
	a.mu.Unlock()
	if cb != nil {
		a.kickConfirmations()
	}
	return func() {
		a.mu.Lock()
		a.activitySink = nil
		a.mu.Unlock()
	}
}

func init() {
	registry.Register("claude", Factory)
}

// SessionIDForPID reads the sessionId of the interactive session with this
// pid from ~/.claude/sessions, the same registry the adapter attaches
// through. amq-remote attach uses it to verify the session it pins is the
// one it runs in.
func SessionIDForPID(pid int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	reg, err := readSessionRegistry(home, pid)
	if err != nil {
		return "", err
	}
	if reg == nil {
		return "", fmt.Errorf("no Claude session registry entry for pid %d", pid)
	}
	return reg.SessionID, nil
}
