package adapter

import (
	"context"
	"errors"
	"fmt"
)

var ErrTargetNotFound = errors.New("adapter target not found")

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

func (r Registry) Get(name string) (Adapter, error) {
	adapter, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q", name)
	}
	return adapter, nil
}
