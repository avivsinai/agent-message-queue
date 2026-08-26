package adapter

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrTargetNotFound = errors.New("adapter target not found")
	// ErrTargetDegraded means an adapter could not safely establish physical
	// ownership. Supervisors must retry without mutating durable registry
	// bookkeeping because the uncertainty may be transient.
	ErrTargetDegraded = errors.New("adapter target ownership degraded")
	// ErrInjectUncertain means text already landed and Enter/submit failed.
	// App.Run prints this to stderr so inject-via can map it without a typed
	// error crossing the child process boundary. Never replay the payload.
	ErrInjectUncertain = errors.New("AMQ_INJECT_PROGRESS=uncertain")
	// ErrGUIAdapterNotReady keeps GUI seats out of the generic wake path until
	// their capability gate has live evidence. A refusal is not a weaker
	// delivery claim.
	ErrGUIAdapterNotReady = errors.New("GUI wake adapter is not ready")
)

type Adapter interface {
	Name() string
	Probe(ctx context.Context, target string) error
	Inject(ctx context.Context, target string, payload string) error
}

type Discoverer interface {
	Discover(ctx context.Context) (string, error)
}

type TargetNormalizer interface {
	NormalizeTarget(target string) (string, error)
}

// CapabilityDeclarer lets an adapter publish its honest delivery vector. An
// adapter that does not implement it is treated as UnknownCapability()
// (weakest on every ordered axis and requires a human), so it is refused
// unless the caller explicitly tolerates a human-required seat. This keeps an
// adapter that forgets to declare a Capability from masquerading as an
// unattended full-strength seat.
type CapabilityDeclarer interface {
	Capability() Capability
}

// TargetCapabilityDeclarer is implemented by adapters whose capability vector
// depends on the resolved target (for example, a deep-link adapter that can
// prefill either a new session or an exact existing conversation). When an
// adapter implements this, the registration gate prefers
// CapabilityForTarget(target) over the target-blind Capability() so a caller
// requesting an existing-exact session is refused a new-only target rather
// than silently downgraded. An error from CapabilityForTarget fails closed.
type TargetCapabilityDeclarer interface {
	CapabilityForTarget(target string) (Capability, error)
}

// TargetInventory is a point-in-time existence snapshot. Implementations must
// return ErrTargetNotFound only when absence is proven by the snapshot; parse,
// transport, and permission failures remain ambiguous errors. When ownership
// is degraded, callers remain fail-closed unless a concrete adapter-specific
// capability proves that the same immutable inventory also established a
// different physical identity. The generic interface deliberately exposes no
// constructor for such a capability.
type TargetInventory interface {
	Probe(target string) error
	OwnershipKey(target string) (string, error)
}

// OwnershipContext carries optional trust the caller has already established
// about a candidate target. Only the registration preflight may populate it:
// the target it is registering was discovered from the live local surface, so
// it is live by construction and can break an otherwise-ambiguous physical
// ownership tie. Supervisor and inject passes pass the zero value, which trusts
// nothing.
type OwnershipContext struct {
	// TrustedTarget is the adapter-native target string (e.g. a cmux surface
	// target) the caller has proven live. Empty means no trusted candidate.
	TrustedTarget string
}

// InventoryProvider lets the supervisor inventory an adapter once per pass
// instead of spawning one probe process for every registry entry. The
// OwnershipContext lets the registration preflight pass a trusted-live
// candidate; other callers pass the zero value.
type InventoryProvider interface {
	Inventory(ctx context.Context, own OwnershipContext) (TargetInventory, error)
}

type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) Registry {
	out := Registry{adapters: map[string]Adapter{}}
	for _, adapter := range adapters {
		out.adapters[adapter.Name()] = adapter
	}
	return out
}

func DefaultRegistry() Registry {
	// claude-desktop and codex-app are registered and reachable behind the
	// capability gate in app.registerWithOptions: a caller gets one only by
	// explicitly accepting a requires-human seat (--accept-requires-human) with
	// delivery/session minima at or below what the target declares. With the
	// default zero-value minimum both are refused automatically, and a
	// submitted/unattended caller is refused too. (The Codex app's dead
	// execute-javascript path stays dead — issue #640 — but its live codex://
	// deep-link prefill seat is shipped here.)
	//
	// codex-queue is the honest submitted GUI/TUI seat: it enqueues into a
	// thread that already has an active writer. It satisfies a submitted +
	// existing-exact unattended minimum without --accept-requires-human.
	// claude-print is the honest submitted CLI seat into an exact existing
	// Claude Code session. It satisfies a submitted + existing-exact
	// unattended minimum without --accept-requires-human. CLAUDE_CONFIG_DIR
	// is ambient (docs/wake-lifecycle.md §9.4).
	return NewRegistry(File{}, Ghostty{}, Cmux{recorded: newCmuxOwnershipRecord()}, ClaudeDesktop{}, CodexApp{}, CodexQueue{}, ClaudePrint{})
}

// DefaultRegistryWithLogf returns the production adapter set with non-fatal
// adapter diagnostics wired to logf. Callers that do not own a diagnostic
// stream can continue using DefaultRegistry.
func DefaultRegistryWithLogf(logf func(format string, args ...any)) Registry {
	return NewRegistry(File{}, Ghostty{}, Cmux{Logf: logf, recorded: newCmuxOwnershipRecord()}, ClaudeDesktop{}, CodexApp{}, CodexQueue{}, ClaudePrint{})
}

func (r Registry) Get(name string) (Adapter, error) {
	adapter, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", name)
	}
	return adapter, nil
}
