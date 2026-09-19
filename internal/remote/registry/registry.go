// Package registry is the capability-driven adapter registration seam.
// Each adapter package calls Register in an init(), contributing a Factory
// keyed by its kind name. serve builds its attachment set from the manifest
// via Build, never editing serve itself for a new adapter.
//
// An optional Discoverer interface lets adapters (.11 Codex app-server socket
// discovery) list candidates without editing serve; serve --discover lists
// candidates and attaches nothing.
package registry

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
)

// FactoryConfig is the construction input for one adapter instance.
type FactoryConfig struct {
	// Root is the AMQ root.
	Root string
	// StateDir is the companion's extensions/remote directory.
	StateDir string
	// Target is the target id from the manifest.
	Target string
	// Epoch is test-only for the fake adapter; for all others it is empty
	// and the live epoch is observed from Inspect at attach time.
	Epoch string
	// Config is the opaque adapter-specific JSON config block.
	Config []byte
}

// Factory constructs one core.Attachment from a FactoryConfig.
type Factory func(ctx context.Context, cfg FactoryConfig) (core.Attachment, error)

// Candidate is a discovered adapter that could be attached.
type Candidate struct {
	Kind   string
	Target string
}

// Discoverer optionally lists attachable adapters without attaching. .11's
// Codex socket discovery plugs in here so serve is never edited for discovery.
type Discoverer interface {
	Discover(ctx context.Context, root, stateDir string) ([]Candidate, error)
}

var (
	mu      sync.RWMutex
	factMu  sync.RWMutex
	factors = map[string]Factory{}
	discs   = map[string]Discoverer{}
)

// Register adds a factory under kind. Calling Register twice for the same
// kind panics (init-time wiring error).
func Register(kind string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := factors[kind]; ok {
		panic(fmt.Sprintf("registry: duplicate factory for kind %q", kind))
	}
	if f == nil {
		panic(fmt.Sprintf("registry: nil factory for kind %q", kind))
	}
	factors[kind] = f
}

// RegisterDiscoverer adds a discoverer under kind. Optional.
func RegisterDiscoverer(kind string, d Discoverer) {
	factMu.Lock()
	defer factMu.Unlock()
	discs[kind] = d
}

// Kinds returns the registered factory kinds in sorted order.
func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factors))
	for k := range factors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownKind is returned when a manifest entry names a kind with no
// registered factory.
type ErrUnknownKind struct {
	Kind   string
	Target string
}

func (e *ErrUnknownKind) Error() string {
	return fmt.Sprintf("adapter %q: unknown kind %q (no factory registered)", e.Target, e.Kind)
}

// Outcome is the per-adapter result of Build: either an Attachment or a
// typed Refusal (the factory returned an error). serve registers what built
// and persists refusals as typed data doctor prints. One bad adapter (an
// unreachable Codex thread, a claude stub refusal) never takes down serve.
type Outcome struct {
	// Manifest is the adapter entry this outcome is for.
	Manifest manifest.Adapter
	// Attachment is non-nil when the factory succeeded.
	Attachment core.Attachment
	// Refusal is non-nil when the factory returned an error. It is the typed
	// error the doctor prints; serve continues without this adapter.
	Refusal error
}

// Build materializes attachments from a manifest, returning per-adapter
// outcomes. Flags append to the manifest set (callers merge before Build).
// A target present in both is a usage error caught by manifest.Validate.
// Build never fails on a single adapter: a factory error becomes a typed
// refusal in the Outcome, not a fatal return. An unknown kind is also a
// refusal (not a fatal error), so serve can persist it and continue.
func Build(ctx context.Context, root, stateDir string, f manifest.File) []Outcome {
	outcomes := make([]Outcome, 0, len(f.Adapters))
	for _, a := range f.Adapters {
		mu.RLock()
		fac, ok := factors[a.Kind]
		mu.RUnlock()
		if !ok {
			outcomes = append(outcomes, Outcome{
				Manifest: a,
				Refusal:  &ErrUnknownKind{Kind: a.Kind, Target: a.Target},
			})
			continue
		}
		att, err := fac(ctx, FactoryConfig{
			Root:     root,
			StateDir: stateDir,
			Target:   a.Target,
			Epoch:    a.Epoch,
			Config:   a.Config,
		})
		if err != nil {
			outcomes = append(outcomes, Outcome{Manifest: a, Refusal: err})
			continue
		}
		outcomes = append(outcomes, Outcome{Manifest: a, Attachment: att})
	}
	return outcomes
}

// Discover lists candidates from all registered discoverers. serve --discover
// calls this and attaches nothing.
func Discover(ctx context.Context, root, stateDir string) ([]Candidate, error) {
	factMu.RLock()
	kinds := make([]string, 0, len(discs))
	for k := range discs {
		kinds = append(kinds, k)
	}
	factMu.RUnlock()
	sort.Strings(kinds)
	var out []Candidate
	for _, k := range kinds {
		factMu.RLock()
		d := discs[k]
		factMu.RUnlock()
		cands, err := d.Discover(ctx, root, stateDir)
		if err != nil {
			return nil, fmt.Errorf("discover %s: %w", k, err)
		}
		out = append(out, cands...)
	}
	return out, nil
}
