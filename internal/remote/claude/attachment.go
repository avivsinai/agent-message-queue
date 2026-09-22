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
	"io"
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
	return &Attachment{
		target:       target,
		cfg:          cfg,
		home:         home,
		runs:         map[requests.Key]*runRecord{},
		cancelIntent: map[requests.Key]bool{},
		ctx:          context.Background(),
	}, nil
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
// the state-leaf lstat refusal. Instead the read is itself bounded and
// hardened: lstat gate (regular file only), O_NOFOLLOW (where available)
// so the lstat-open race cannot swap in a FIFO, and an io.LimitReader
// capped at maxRegistryBytes+1 so a file that grows between the lstat and
// the read cannot bypass the size bound and balloon RSS under the
// endpoint mutex (r3 P1).
func readSessionRegistry(home string, pid int) (*sessionRegistry, error) {
	path := filepath.Join(claudeSessionsDir(home), strconv.Itoa(pid)+".json")
	// Lstat gate: anything but a regular file is refused. os.ReadFile on a
	// FIFO blocks unbounded until a writer appears, and this read runs
	// under the endpoint's mutex (r2 P1-1).
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no registry entry yet: not an error
		}
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("session registry %s: not a regular file (mode %s); refusing", path, fi.Mode())
	}
	// Size bound BEFORE opening (r3 P1): fi.Size() from the lstat above is
	// the admission check; the LimitReader below is the defense in depth
	// for a file that grows between lstat and read (r3 P2-2 TOCTOU).
	if fi.Size() > maxRegistryBytes {
		return nil, fmt.Errorf("session registry %s: %d bytes exceeds %d; refusing", path, fi.Size(), maxRegistryBytes)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|registryNoFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	if int64(len(raw)) > maxRegistryBytes {
		return nil, fmt.Errorf("session registry %s: larger than %d bytes after read; refusing", path, maxRegistryBytes)
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
	stopSeen      int64
	ctx           context.Context
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
	status := a.observeStatus()
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

// observeStatus is a pure filesystem read: the registry file's status
// field plus a pid liveness check. No child process, no bound that can
// silently exceed its timeout (P0-1).
func (a *Attachment) observeStatus() string {
	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil {
		return "offline"
	}
	if reg == nil {
		// No registry entry (r3: missing-file path) — offline.
		return "offline"
	}
	if alive, err := pidAlive(a.cfg.Pid); err != nil || !alive {
		return "offline"
	}
	// A registry status the process does not update any more is still
	// "unknown" in the projection, never a guessed idle.
	if reg.Status == "" {
		return "unknown"
	}
	return normalizeStatus(reg.Status)
}

func (a *Attachment) now() time.Time { return time.Now() }

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
	a.mu.Unlock()
}

// Subscribe registers the native-event sink (the confirmation poller
// emits status events; the Stop-hook receiver posts terminal events
// through the same sink). Returns the unsubscribe function.
func (a *Attachment) Subscribe(cb func(core.NativeEvent)) func() {
	a.mu.Lock()
	a.eventSink = cb
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.eventSink = nil
		a.mu.Unlock()
	}
}

func init() {
	registry.Register("claude", Factory)
}
