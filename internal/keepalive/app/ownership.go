package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
)

type ownershipRememberer interface {
	RememberOwnership(target, key string)
}

func seedAdapterOwnership(adapters adapter.Registry, entries []registry.Entry) {
	for _, entry := range entries {
		if strings.TrimSpace(entry.OwnershipKey) == "" {
			continue
		}
		selected, err := adapters.Get(entry.Adapter)
		if err != nil {
			continue
		}
		remember, ok := selected.(ownershipRememberer)
		if !ok {
			continue
		}
		remember.RememberOwnership(entry.Target, entry.OwnershipKey)
	}
}

func lookupOwnershipKey(ctx context.Context, probe supervisor.Adapter, target string) string {
	op, ok := probe.(ownershipProbe)
	if !ok {
		return ""
	}
	key, err := op.OwnershipKey(ctx, target)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(key)
}

type fixedProbeError struct {
	err error
}

func (p fixedProbeError) Probe(context.Context, string) error {
	return p.err
}

type passProbe struct {
	selected  adapter.Adapter
	once      sync.Once
	inventory adapter.TargetInventory
	err       error
}

type ownershipProbe interface {
	supervisor.Adapter
	OwnershipKey(ctx context.Context, target string) (string, error)
}

func newPassProbe(selected adapter.Adapter) supervisor.Adapter {
	if _, ok := selected.(adapter.InventoryProvider); !ok {
		return selected
	}
	return &passProbe{selected: selected}
}

func passProbes(entries []registry.Entry, adapters adapter.Registry) map[string]supervisor.Adapter {
	probes := make(map[string]supervisor.Adapter)
	for _, entry := range entries {
		if _, ok := probes[entry.Adapter]; ok {
			continue
		}
		selected, err := adapters.Get(entry.Adapter)
		if err != nil {
			probes[entry.Adapter] = fixedProbeError{err: err}
			continue
		}
		probes[entry.Adapter] = newPassProbe(selected)
	}
	return probes
}

func (p *passProbe) Probe(ctx context.Context, target string) error {
	inventory, err := p.loadInventory(ctx)
	if err != nil {
		return err
	}
	return inventory.Probe(target)
}

func (p *passProbe) OwnershipKey(ctx context.Context, target string) (string, error) {
	inventory, err := p.loadInventory(ctx)
	if err != nil {
		return "", err
	}
	return inventory.OwnershipKey(target)
}

func (p *passProbe) loadInventory(ctx context.Context) (adapter.TargetInventory, error) {
	provider := p.selected.(adapter.InventoryProvider)
	p.once.Do(func() {
		// Supervisor pass: no trusted candidate. The pass may auto-resolve only
		// provably-dead ttys; any other contested tty stays fail-closed.
		p.inventory, p.err = provider.Inventory(ctx, adapter.OwnershipContext{})
		if p.err == nil && p.inventory == nil {
			p.err = errors.New("adapter returned a nil target inventory")
		}
	})
	if p.err != nil {
		return nil, p.err
	}
	return p.inventory, nil
}

type targetOwnerKey struct {
	adapter string
	target  string
}

func targetOwnershipConflicts(file registry.File, adapters adapter.Registry) map[string]error {
	groups := make(map[targetOwnerKey][]registry.Entry)
	for _, entry := range file.Entries {
		selected, err := adapters.Get(entry.Adapter)
		if err != nil {
			continue
		}
		target := strings.TrimSpace(entry.Target)
		if normalizer, ok := selected.(adapter.TargetNormalizer); ok {
			target, err = normalizer.NormalizeTarget(target)
			if err != nil {
				continue
			}
		}
		key := targetOwnerKey{adapter: selected.Name(), target: target}
		groups[key] = append(groups[key], entry)
	}

	conflicts := make(map[string]error)
	for key, owners := range groups {
		if len(owners) < 2 {
			continue
		}
		labels := make([]string, 0, len(owners))
		for _, owner := range owners {
			labels = append(labels, fmt.Sprintf("%s@%s (%s)", owner.Agent, owner.Root, owner.ID))
		}
		err := fmt.Errorf("adapter target ownership collision: adapter=%q target=%q owners=%s", key.adapter, key.target, strings.Join(labels, ", "))
		for _, owner := range owners {
			conflicts[owner.ID] = err
		}
	}
	return conflicts
}

type physicalTargetOwnerKey struct {
	adapter string
	key     string
}

func physicalOwnershipConflicts(ctx context.Context, file registry.File, probes map[string]supervisor.Adapter) map[string]error {
	groups := make(map[physicalTargetOwnerKey][]registry.Entry)
	conflicts := make(map[string]error)
	for _, entry := range file.Entries {
		probe, ok := probes[entry.Adapter].(ownershipProbe)
		if !ok {
			continue
		}
		key, err := probe.OwnershipKey(ctx, entry.Target)
		if errors.Is(err, adapter.ErrTargetNotFound) {
			continue
		}
		if err != nil {
			conflicts[entry.ID] = fmt.Errorf("resolve physical target ownership for adapter=%q target=%q: %w", entry.Adapter, entry.Target, err)
			continue
		}
		groups[physicalTargetOwnerKey{adapter: entry.Adapter, key: key}] = append(
			groups[physicalTargetOwnerKey{adapter: entry.Adapter, key: key}], entry,
		)
	}
	for key, owners := range groups {
		if len(owners) < 2 {
			continue
		}
		labels := make([]string, 0, len(owners))
		for _, owner := range owners {
			labels = append(labels, fmt.Sprintf("%s@%s (%s, target=%s)", owner.Agent, owner.Root, owner.ID, owner.Target))
		}
		err := fmt.Errorf(
			"physical target ownership collision: adapter=%q identity=%q owners=%s; existing wakes are not retired automatically",
			key.adapter, key.key, strings.Join(labels, ", "),
		)
		for _, owner := range owners {
			conflicts[owner.ID] = err
		}
	}
	return conflicts
}

func mergeOwnershipConflicts(destination, source map[string]error) {
	for id, err := range source {
		if _, exists := destination[id]; !exists {
			destination[id] = err
		}
	}
}
