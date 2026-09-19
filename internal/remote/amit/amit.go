// Package amit is the Amit (pi) adapter factory. It registers the
// kind:"amit" factory under the named-factory registry so a manifest entry
// `{"kind":"amit","target":"amit","config":{"extension":"amq-bridge"}}`
// builds an AmitAttachment without editing serve.
//
// The Amit adapter is extension-only: it does not spawn, own, or supervise
// the Amit process (ADR invariant 1 — up supervises serve only; serve never
// owns a harness). The companion on the Amit host runs an extension inside
// Amit itself (pi extension event bus, pi.events); this attachment
// correlates submits through that extension's event stream and a
// getEntries()-tail diff.
//
// Evidence: pi's sendUserMessage returns void and swallows rejections — a
// lost submit is indistinguishable from a never-submitted key. Every
// terminal disposition therefore comes from the extension event stream or
// the session-entry diff, never from the send primitive, and a key the
// adapter retains nothing about is EvidenceUnknown, never EvidenceNone.
package amit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// config is the adapter-specific config block for an amit manifest entry.
type config struct {
	// Extension names the Amit-side extension that owns the input boundary
	// and publishes the event stream this attachment correlates against.
	Extension string `json:"extension"`
}

// SessionSource is the durable observation seam this adapter reads. On the
// Amit host it is backed by the amq-bridge extension (pi.events + session
// entries); tests inject a fake. The adapter never spawns Amit.
type SessionSource interface {
	// Subscribe delivers extension events until the returned function is
	// called. Events carry the extension's observations about session
	// entries and turn lifecycle.
	Subscribe(fn func(ExtEvent)) func()
	// Entries returns the session entries the extension observed so far,
	// oldest first. The adapter tail-diffs successive calls to correlate
	// submits with their outcomes.
	Entries() []ExtEntry
	// Status returns the current session status as observed by the
	// extension: "idle", "busy", or "offline".
	Status() string
}

// ExtEvent is one event from the Amit-side extension.
type ExtEvent struct {
	// Type discriminates the observation: "user_message" (a user message
	// entry landed in the session), "turn_start", "turn_end" (with
	// Terminal/ErrText), "status".
	Type string
	// ClientRef echoes protocol.EncodeRef of the request the extension
	// correlated this observation with (empty for status events).
	ClientRef string
	// Text is the message text for user_message events.
	Text string
	// Terminal is the terminal state name for turn_end: "completed",
	// "failed", "cancelled".
	Terminal string
	// ErrText is the failure message for a failed turn.
	ErrText string
}

// ExtEntry is one session entry observed by the extension.
type ExtEntry struct {
	// Kind is the entry kind: "user_message", "agent_message", or a
	// per-request refusal "status" line (only those carry a ClientRef).
	Kind string
	// ClientRef is protocol.EncodeRef of the correlated request when the
	// extension tagged the entry (user messages and per-request refusals
	// carry the ref).
	ClientRef string
	// Text is the entry text.
	Text string
	// Status is the refusal/status value for per-request status entries
	// (e.g. "refused_busy", "refused" with a steer_disabled_v1 reason).
	Status string
}

// run is one bound request in flight.
type run struct {
	key       requests.Key
	epoch     string
	runID     string
	state     protocol.State
	text      strings.Builder
	errText   string
	nativeRef string
	acked     bool
	seenTurn  bool
}

// Attachment implements core.Attachment over the extension event stream. It
// never touches the local editor and owns no process.
type Attachment struct {
	mu      sync.Mutex
	target  string
	epoch   string
	source  SessionSource
	extName string
	offline bool

	runs      map[requests.Key]*run
	order     []requests.Key // bind order, for bounded pruning
	entryTail int            // how many of source.Entries() have been consumed
	listeners map[int]func(core.NativeEvent)
	nextID    int
	now       func() time.Time
	// deliverDir is the extension inbox this adapter writes submits into
	// (empty only in tests that inject the source directly).
	deliverDir string
}

// maxRetainedBounds the retained run map and entry consumption window; both
// are process-local correlation state, bounded so a long-lived attachment
// cannot grow them without limit.
const maxRetained = 256

// New builds an attachment over source for the manifest target. epoch is
// empty in production (the live epoch is observed from Inspect); tests may
// pin one.
func New(target, epoch string, source SessionSource) (*Attachment, error) {
	if target == "" {
		return nil, fmt.Errorf("amit: target is required")
	}
	if source == nil {
		return nil, fmt.Errorf("amit: session source is required")
	}
	a := &Attachment{
		target:    target,
		epoch:     epoch,
		source:    source,
		runs:      map[requests.Key]*run{},
		listeners: map[int]func(core.NativeEvent){},
		now:       time.Now,
	}
	// Drain the pre-existing entry tail so a submit is only correlated with
	// entries that land after it.
	a.entryTail = len(source.Entries())
	return a, nil
}

// Factory builds an AmitAttachment from a registry.FactoryConfig.
func Factory(ctx context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("parse amit config: %w", err)
		}
	}
	if c.Extension == "" {
		return nil, fmt.Errorf("amit config: extension is required")
	}
	src, err := Dial(ctx, cfg, c.Extension)
	if err != nil {
		return nil, err
	}
	epoch := cfg.Epoch
	att, err := New(cfg.Target, epoch, src)
	if err != nil {
		return nil, err
	}
	att.extName = c.Extension
	att.deliverDir = deliverPath(cfg.Root, c.Extension)
	return att, nil
}

func init() {
	registry.Register("amit", Factory)
}
