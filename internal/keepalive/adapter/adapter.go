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

// DegradedOwnershipError reports uncertain ownership while retaining a
// physical identity that the inventory established independently of the
// uncertain owner. Callers may compare OwnershipKey with a successfully
// resolved candidate key, but must stay fail-closed when no such key is
// available.
type DegradedOwnershipError struct {
	OwnershipKey string
	Detail       string
}

func (e *DegradedOwnershipError) Error() string {
	if e.Detail == "" {
		return ErrTargetDegraded.Error()
	}
	return ErrTargetDegraded.Error() + ": " + e.Detail
}

func (*DegradedOwnershipError) Unwrap() error {
	return ErrTargetDegraded
}

// DegradedOwnershipKey returns a physical key only when an
// ErrTargetDegraded value explicitly carries one. A false result means the
// physical identity itself is uncertain and consumers must not infer that the
// target is unrelated to another owner.
func DegradedOwnershipKey(err error) (string, bool) {
	var degraded *DegradedOwnershipError
	if !errors.As(err, &degraded) || degraded.OwnershipKey == "" {
		return "", false
	}
	return degraded.OwnershipKey, true
}

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
// is degraded but the physical identity is certain, implementations may return
// DegradedOwnershipError so callers can compare that identity without treating
// the uncertain target as healthy.
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
