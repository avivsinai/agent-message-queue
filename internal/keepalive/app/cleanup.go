package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

// beforeGCRegistryLoadForTest runs after gc holds the registration lease and
// immediately before Load. Tests use it to prove Load is lock-serialized.
var beforeGCRegistryLoadForTest func()

type gcEntryResult struct {
	ID            string    `json:"id"`
	Root          string    `json:"root"`
	Agent         string    `json:"agent"`
	Adapter       string    `json:"adapter"`
	Target        string    `json:"target"`
	DetachedSince time.Time `json:"detached_since,omitempty"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	PID           int       `json:"pid,omitempty"`
}

type gcResult struct {
	Applied        bool            `json:"applied"`
	MinDetachedAge string          `json:"min_detached_age"`
	Entries        []gcEntryResult `json:"entries"`
}

// gc classifies proven-detached registry entries. Load, probe, retire, and
// forget run under the registration lease so a concurrent reattach cannot
// start a new generation and then have this pass retire it from a stale
// snapshot. With --apply it retires each candidate's exact previous inject-via
// wake via amq wake retire and forgets only the entries AMQ positively
// confirmed retired; any refusal leaves the entry in place for the next pass
// and surfaces an error.
func (a App) gc(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	amqPath := fs.String("amq", "amq", "amq executable path for --apply retirement")
	self := fs.String("self", executablePath(), "amq-keepalive executable path expected as the wake's --inject-via")
	minDetachedAge := fs.Duration("min-detached-age", 24*time.Hour, "minimum proven-detached age before cleanup")
	apply := fs.Bool("apply", false, "retire proven-detached candidates via amq wake retire and forget confirmed entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	if *minDetachedAge < 0 {
		return errors.New("--min-detached-age must not be negative")
	}

	var retirer wakeRetirer = amq.NewCLI(*amqPath)
	store := registry.New(*registryPath)
	result := gcResult{Applied: *apply, MinDetachedAge: minDetachedAge.String()}
	var applyErrs []error
	err := store.WithRegistrationLockContext(ctx, func() error {
		if beforeGCRegistryLoadForTest != nil {
			beforeGCRegistryLoadForTest()
		}
		file, err := store.Load()
		if err != nil {
			return err
		}
		adapters := a.adapterRegistry()
		probes := passProbes(file.Entries, adapters)
		conflicts := targetOwnershipConflicts(file, adapters)
		mergeOwnershipConflicts(conflicts, physicalOwnershipConflicts(ctx, file, probes))
		now := time.Now().UTC()
		for _, entry := range file.Entries {
			item := gcEntryResult{
				ID: entry.ID, Root: entry.Root, Agent: entry.Agent, Adapter: entry.Adapter,
				Target: entry.Target, DetachedSince: entry.DetachedSince,
			}
			result.Entries = append(result.Entries, item)
			index := len(result.Entries) - 1
			if entry.State != registry.StateDetached {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = "entry is not detached"
				continue
			}
			if entry.DetachedSince.IsZero() {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = "detached_since is not yet established by the fixed supervisor"
				continue
			}
			if age := now.Sub(entry.DetachedSince); age < *minDetachedAge {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = fmt.Sprintf("detached for %s; minimum is %s", age.Round(time.Second), minDetachedAge.String())
				continue
			}
			if conflictErr, ok := conflicts[entry.ID]; ok {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = conflictErr.Error()
				continue
			}
			selected, selectErr := adapters.Get(entry.Adapter)
			if selectErr != nil {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = selectErr.Error()
				continue
			}
			target, normalizeErr := normalizedTarget(selected, entry.Target)
			if normalizeErr != nil {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = normalizeErr.Error()
				continue
			}
			var generation string
			if *apply {
				observed, observeErr := observeWakeGeneration(ctx, retirer, entry.Root, entry.Agent)
				if observeErr != nil {
					result.Entries[index].Status = "skipped"
					result.Entries[index].Reason = observeErr.Error()
					continue
				}
				generation = observed
			}
			probeErr := probes[entry.Adapter].Probe(ctx, target)
			if probeErr == nil {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = "adapter target currently exists"
				continue
			}
			if !errors.Is(probeErr, adapter.ErrTargetNotFound) {
				result.Entries[index].Status = "skipped"
				result.Entries[index].Reason = "target absence is ambiguous: " + probeErr.Error()
				continue
			}
			result.Entries[index].Status = "candidate"
			if !*apply {
				continue
			}
			// Retire the exact previous inject-via identity. AMQ acts only on an
			// identity-confirmed live wake with this saved target (or its
			// exactly-bound proven-stale lock); any refusal leaves the entry.
			if _, retireErr := retirer.RetireWake(ctx, amq.RetireWakeRequest{
				Root:       entry.Root,
				Me:         entry.Agent,
				InjectVia:  *self,
				Adapter:    entry.Adapter,
				Target:     target,
				Generation: generation,
			}); retireErr != nil {
				result.Entries[index].Status = "error"
				result.Entries[index].Reason = retireErr.Error()
				applyErrs = append(applyErrs, fmt.Errorf("retire %s@%s wake: %w", entry.Agent, entry.Root, retireErr))
				continue
			}
			removed, forgetErr := store.ForgetIfUnchanged(entry)
			switch {
			case forgetErr != nil:
				result.Entries[index].Status = "error"
				result.Entries[index].Reason = "wake retired but registry forget failed: " + forgetErr.Error()
				applyErrs = append(applyErrs, fmt.Errorf("forget %s@%s registry entry after retire: %w", entry.Agent, entry.Root, forgetErr))
			case !removed:
				result.Entries[index].Status = "retired"
				result.Entries[index].Reason = "wake retired; registry entry changed concurrently and was left in place"
			default:
				result.Entries[index].Status = "retired"
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := printJSON(a.Stdout, result); err != nil {
		return err
	}
	if len(applyErrs) > 0 {
		return errors.Join(applyErrs...)
	}
	return nil
}

func normalizedTarget(selected adapter.Adapter, target string) (string, error) {
	target = strings.TrimSpace(target)
	if normalizer, ok := selected.(adapter.TargetNormalizer); ok {
		return normalizer.NormalizeTarget(target)
	}
	if target == "" {
		return "", errors.New("adapter target is empty")
	}
	return target, nil
}

func (a App) forget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	id := fs.String("id", "", "registry entry id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	store := registry.New(*registryPath)
	removed := false
	err := store.WithRegistrationLockContext(ctx, func() error {
		var err error
		removed, err = store.Forget(*id)
		return err
	})
	if err != nil {
		return err
	}
	return printJSON(a.Stdout, map[string]any{"removed": removed})
}

func (a App) retireSession(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("retire-session", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	rootFlag := fs.String("root", "", "exact AMQ session root")
	adapterName := fs.String("adapter", "cmux", "adapter name")
	agentsFlag := fs.String("agents", "codex,claude", "comma-separated required agent handles")
	amqPath := fs.String("amq", "amq", "amq executable path for retirement")
	self := fs.String("self", executablePath(), "amq-keepalive executable path expected as each wake's --inject-via")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	if strings.TrimSpace(*rootFlag) == "" {
		return errors.New("--root is required")
	}
	root, err := canonicalExistingPath(*rootFlag)
	if err != nil {
		return fmt.Errorf("resolve --root: %w", err)
	}
	agents, err := parseRequiredAgents(*agentsFlag)
	if err != nil {
		return err
	}

	store := registry.New(*registryPath)
	return store.WithRegistrationLockContext(ctx, func() error {
		file, err := store.Load()
		if err != nil {
			return err
		}
		entries := make([]registry.Entry, 0, len(agents))
		for _, agent := range agents {
			matches := make([]registry.Entry, 0, 1)
			for _, entry := range file.Entries {
				entryRoot, pathErr := canonicalExistingPath(entry.Root)
				if pathErr != nil {
					continue
				}
				if entryRoot == root && entry.Adapter == *adapterName && entry.Agent == agent {
					matches = append(matches, entry)
				}
			}
			if len(matches) != 1 {
				return fmt.Errorf("expected exactly one %s registry entry for agent %s at %s, found %d", *adapterName, agent, root, len(matches))
			}
			entries = append(entries, matches[0])
		}

		adapters := a.adapterRegistry()
		selected, err := adapters.Get(*adapterName)
		if err != nil {
			return err
		}
		// Precondition: every target must be independently proven absent before
		// any wake is retired. entries keep their original stored form so the
		// post-retire forget matches exactly; normalized targets feed the probe
		// and the retire identity.
		targets := make([]string, len(entries))
		generations := make([]string, len(entries))
		var retirer wakeRetirer = amq.NewCLI(*amqPath)
		for i := range entries {
			generation, observeErr := observeWakeGeneration(ctx, retirer, entries[i].Root, entries[i].Agent)
			if observeErr != nil {
				return fmt.Errorf("observe %s wake generation: %w", entries[i].Agent, observeErr)
			}
			generations[i] = generation
			target := entries[i].Target
			if normalizer, ok := selected.(adapter.TargetNormalizer); ok {
				normalized, normalizeErr := normalizer.NormalizeTarget(target)
				if normalizeErr != nil {
					return fmt.Errorf("normalize target for %s: %w", entries[i].Agent, normalizeErr)
				}
				target = normalized
			}
			probeErr := selected.Probe(ctx, target)
			if probeErr == nil {
				return fmt.Errorf("refusing to retire %s wake: adapter target %s still exists", entries[i].Agent, target)
			}
			if !errors.Is(probeErr, adapter.ErrTargetNotFound) {
				return fmt.Errorf("refusing to retire %s wake because target absence is not proven: %w", entries[i].Agent, probeErr)
			}
			targets[i] = target
		}

		// Retire each identity-confirmed wake and forget only the rows AMQ
		// confirmed retired. A refusal leaves that row for the next pass.
		var retireErrs []error
		for i := range entries {
			if _, retireErr := retirer.RetireWake(ctx, amq.RetireWakeRequest{
				Root:       entries[i].Root,
				Me:         entries[i].Agent,
				InjectVia:  *self,
				Adapter:    entries[i].Adapter,
				Target:     targets[i],
				Generation: generations[i],
			}); retireErr != nil {
				retireErrs = append(retireErrs, fmt.Errorf("retire %s wake: %w", entries[i].Agent, retireErr))
				continue
			}
			if _, forgetErr := store.ForgetIfUnchanged(entries[i]); forgetErr != nil {
				retireErrs = append(retireErrs, fmt.Errorf("forget %s registry entry after retire: %w", entries[i].Agent, forgetErr))
			}
		}
		if len(retireErrs) > 0 {
			return errors.Join(retireErrs...)
		}
		return nil
	})
}

func canonicalExistingPath(path string) (string, error) {
	return registry.CanonicalRoot(path)
}

func parseRequiredAgents(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	agents := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		agent := strings.TrimSpace(part)
		if agent == "" {
			return nil, errors.New("--agents must contain non-empty handles")
		}
		if seen[agent] {
			return nil, fmt.Errorf("--agents contains duplicate handle %q", agent)
		}
		seen[agent] = true
		agents = append(agents, agent)
	}
	if len(agents) == 0 {
		return nil, errors.New("--agents is required")
	}
	return agents, nil
}
