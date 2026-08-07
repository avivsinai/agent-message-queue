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
	return NewRegistry(File{}, Ghostty{}, Cmux{})
}

// DefaultRegistryWithLogf returns the production adapter set with non-fatal
// adapter diagnostics wired to logf. Callers that do not own a diagnostic
// stream can continue using DefaultRegistry.
func DefaultRegistryWithLogf(logf func(format string, args ...any)) Registry {
	return NewRegistry(File{}, Ghostty{}, Cmux{Logf: logf})
}

func (r Registry) Get(name string) (Adapter, error) {
	adapter, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", name)
	}
	return adapter, nil
}
