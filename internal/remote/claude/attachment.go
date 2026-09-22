// Package claude is the Claude Code adapter factory (bead 611.12, PR1).
// It registers the kind:"claude" factory so a manifest entry
// `{"kind":"claude","target":"<pid>","config":{...}}` builds a
// claudeAttachment without editing serve.
//
// PR1 scope (architect ruling 10:59Z, binding): the adapter skeleton,
// inspect via `claude agents --json` plus the transcript tail reader, the
// opt-in user-level Stop-hook installer, and an HONEST capability
// projection — submit is advertised false and refusal-only until PR2 wires
// the pinned 611.2 wire (docs/research/r0-03-cc-socket-wire-capture.md,
// merged at 17bff18). No keystrokes, no print child, no `claude -p`
// fallback, no process ownership (ADR invariant 1).
//
// Evidence vocabulary (architect ruling 10:59Z; "delivered" does not exist
// in the schema — #822 removed it): a socket ack alone is TENTATIVE
// (bound, ownership not proven); the transcript user line is SUBMITTED;
// the assistant turn starting (Stop-hook post or transcript assistant
// line) is ADMITTED. PR1 issues none of these over a wire: submit refuses
// before any side effect.
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
	// Home overrides the Claude home for tests; empty = os.UserHomeDir().
	Home string `json:"home,omitempty"`
	// ClaudeBin is the CLI binary for roster polling; empty = "claude".
	ClaudeBin string `json:"claude_bin,omitempty"`
	// StopHook opts in to the user-level Stop-hook installer at attach time.
	// Installation is idempotent and never touches other hooks; Uninstall
	// restores the prior settings byte-for-byte. Never invoked without the
	// explicit operator opt-in (architect ruling 10:59Z: ~/.claude is shared
	// by every agent on the machine).
	StopHook bool `json:"stop_hook,omitempty"`
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
}

// rosterEntry is the subset of `claude agents --json` the adapter reads.
type rosterEntry struct {
	Pid       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
}

// Attachment implements core.Attachment over the Claude Code surfaces.
// It never injects keystrokes, never spawns a print child, and owns no
// process (ADR invariant 1). PR1 tracks no runs (submit refuses before
// any side effect); PR2 adds the run table alongside the socket client.
type Attachment struct {
	target string
	cfg    config
	home   string
}

// ErrSubmitUnwired is the refusal Submit returns in PR1: the pinned 611.2
// wire is merged (17bff18) but not yet wired (PR2). The refusal is the
// honest projection — no capability is implied, no side effect occurs.
var ErrSubmitUnwired = errors.New("claude submit is not wired until 611.12 PR2 (socket delivery over the 611.2 pinned wire); this refusal is the capability projection, not a transient error")

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
	target := cfgTarget(cfg.Pid, reg)
	return &Attachment{
		target: target,
		cfg:    cfg,
		home:   home,
	}, nil
}

func cfgTarget(pid int, reg *sessionRegistry) string {
	if reg != nil && reg.Name != "" {
		return reg.Name
	}
	return "claude:" + strconv.Itoa(pid)
}

func claudeSessionsDir(home string) string {
	return filepath.Join(home, ".claude", "sessions")
}

// readSessionRegistry reads ~/.claude/sessions/<pid>.json. The file is
// read through os.ReadFile: it is a Claude-internal registry file, not an
// AMQ state leaf, so the state-leaf lstat rule does not apply here.
func readSessionRegistry(home string, pid int) (*sessionRegistry, error) {
	path := filepath.Join(claudeSessionsDir(home), strconv.Itoa(pid)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
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

// transcriptPath maps cwd+sessionId to ~/.claude/projects/<slug>/<sessionId>.jsonl
// (slug = cwd with '/' and non-allowed chars replaced by '-'; observed rule
// in docs/research/r0-03 and seats/harness-surfaces.md §2.1).
func transcriptPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", slugifyCwd(cwd), sessionID+".jsonl")
}

func slugifyCwd(cwd string) string {
	replacer := strings.NewReplacer("/", "-", ".", "-", "_", "-")
	return replacer.Replace(cwd)
}

// Inspect implements core.Attachment: the honest projection. Status comes
// from the roster poll; capabilities are the design-permitted set with
// submit FALSE until PR2 (architect ruling 10:59Z: stated as refusal-only
// in the PR body). Evidence is nil — PR1 issues no submit evidence class
// at all, and a nil projection fails closed under any caller floor
// (endpoint gate, internal/remote/core/endpoint.go).
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

func (a *Attachment) now() time.Time { return time.Now() }

// observeStatus polls `claude agents --json` for the target pid; the
// session registry is the fallback when the roster does not list it (e.g.
// a --bg session owned by a different CLI version). A failed poll is
// status "unknown" with attachment "live" only when the registry file
// still exists — never a guessed "idle".
func (a *Attachment) observeStatus() string {
	entries, err := agentRoster(a.cfg.ClaudeBin)
	if err == nil {
		for _, e := range entries {
			if e.Pid == a.cfg.Pid {
				if e.Status == "" {
					return "unknown"
				}
				return e.Status
			}
		}
	}
	// Roster miss: fall back to liveness of the registry file.
	if _, err := readSessionRegistry(a.home, a.cfg.Pid); err != nil {
		return "offline"
	}
	return "unknown"
}

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

// CancelExact implements core.Attachment: refusal-only in PR1. Claude Code
// has no user-level interrupt without keystrokes (pinned evidence,
// docs/remote-compat.md §3), so cancellation is unsupported even in PR2.
func (a *Attachment) CancelExact(key requests.Key, _ string) (core.CancelEvidence, error) {
	return core.CancelEvidence{}, nil
}

// Respond implements core.Attachment: no approval/question seam exists
// (docs/remote-compat.md §3; Remote Control is policy-disabled). The
// endpoint translates a false AnswerQuestion capability into
// already_resolved before this is reached.
func (a *Attachment) Respond(_ requests.Key, _, _, _ string) (protocol.Code, error) {
	return "", nil
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
