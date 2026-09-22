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
	"strings"
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

// ErrSubmitUnwired is the refusal Submit returns in PR1: the pinned 611.2
// wire is merged (17bff18) but not yet wired (PR2). The refusal is the
// honest projection — no capability is implied, no side effect occurs.
var ErrSubmitUnwired = errors.New("claude submit is not wired until 611.12 PR2 (socket delivery over the 611.2 pinned wire); this refusal is the capability projection, not a transient error")

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
		target: target,
		cfg:    cfg,
		home:   home,
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

// readSessionRegistry reads ~/.claude/sessions/<pid>.json. The file is
// read through os.ReadFile: it is a Claude-internal registry file, not an
// AMQ state leaf, so the state-leaf lstat rule does not apply to this
// read.
func readSessionRegistry(home string, pid int) (*sessionRegistry, error) {
	path := filepath.Join(claudeSessionsDir(home), strconv.Itoa(pid)+".json")
	// The registry path is local-process-writable (trust boundary stated
	// above): lstat before opening — anything but a regular file is
	// refused. os.ReadFile on a FIFO blocks unbounded until a writer
	// appears, and this read runs under the endpoint's mutex (r2 P1-1).
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("session registry %s: not a regular file (mode %s); refusing", path, fi.Mode())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session registry %s: %w", path, err)
	}
	// Bound the read size: a regular file is still local-process-writable,
	// and an unbounded read of a huge file under the endpoint mutex is the
	// same freeze shape (r2 P1-1). A real registry entry is < 1 KiB.
	if len(raw) > maxRegistryBytes {
		return nil, fmt.Errorf("session registry %s: %d bytes exceeds %d; refusing", path, len(raw), maxRegistryBytes)
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

// transcriptPath maps cwd+sessionId to ~/.claude/projects/<slug>/<sessionId>.jsonl
// (slug = cwd with every non-alphanumeric character replaced by '-' — the
// observed rule on this machine, e.g. "Application Support" →
// "Application-Support"; review-852-r1 P2-2).
func transcriptPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", slugifyCwd(cwd), sessionID+".jsonl")
}

func slugifyCwd(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
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
}

// Inspect implements core.Attachment: the honest projection. Status is
// the registry file's own status field, normalized to the projection
// vocabulary {idle,busy,unknown,offline} (P2-1, mirroring codex
// threadStatus); the pid is liveness-checked (P2-4 — a stale registry
// left by a crashed session must not read "live" forever). Capabilities
// are the design-permitted set with submit FALSE until PR2. Evidence is
// nil — PR1 issues no submit evidence class at all, and a nil projection
// fails closed under any caller floor (endpoint gate).
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
		// Honest projection (ruling 10:59Z): cancel_request/approve_tool/
		// answer_question/steer are false — no interrupt seam exists without
		// keystrokes (docs/remote-compat.md §3 rows) — and submit is false
		// until PR2 wires the pinned socket delivery.
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: false, CancelRequest: false,
			ApproveTool: false, AnswerQuestion: false, Steer: false,
			Terminal: "unavailable",
		},
		Evidence:   nil, // nothing provable until PR2; fails closed under a floor
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
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	return core.Admission{
		Code:    protocol.CodeUnsupported,
		Message: ErrSubmitUnwired.Error(),
	}, nil
}

// Lookup implements core.Attachment. PR1 has no submit path, so the
// adapter never created a run: there is nothing the adapter can speak for.
// Following the amit posture for a key the adapter retains nothing about:
// Known=true with class EvidenceUnknown — never EvidenceNone (which would
// claim a real admission primitive proved non-admission) and never a
// guessed class.
func (a *Attachment) Lookup(key requests.Key, _ string) (core.Evidence, error) {
	return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
}

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
func (a *Attachment) AcknowledgeResult(_ requests.Key, _, _ string) {}

// Subscribe implements core.Attachment: PR1 has no native event stream to
// forward (the Stop hook posts into PR2's evidence path, not a stream).
func (a *Attachment) Subscribe(_ func(core.NativeEvent)) func() {
	return func() {}
}

func init() {
	registry.Register("claude", Factory)
}
