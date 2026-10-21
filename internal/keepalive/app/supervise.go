package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
)

const adapterLogInterval = 5 * time.Minute

// companionSupervisedAdapter is the registry adapter name used by
// amq-remote up: entries with it are supervised by their own companion
// process and are skipped by reconcile (see superviseOnce).
const companionSupervisedAdapter = "remote"

type adapterLogState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (a App) supervise(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", selfUpgradeDefaultLocator(), "amq-keepalive executable path for --inject-via")
	once := fs.Bool("once", false, "run one supervisor pass")
	interval := fs.Duration("interval", time.Minute, "supervisor interval")
	wakeTimeout := fs.Duration("wake-ready-timeout", 10*time.Second, "maximum time to wait for amq wake readiness")
	noSelfUpgrade := fs.Bool("no-self-upgrade", false, "disable automatic replacement by a newer keepalive image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	if *interval <= 0 {
		return errors.New("--interval must be greater than zero")
	}
	settingsPath, err := keepaliveSettingsPath(*registryPath)
	if err != nil {
		return err
	}
	selfUpgrade := newSelfUpgradeController(*registryPath, *self, a.SelfVersion, !*noSelfUpgrade)
	selfUpgrade.settingsPath = settingsPath
	if selfUpgrade.enabled && !selfUpgrade.eligible && selfUpgrade.reason != "" {
		_, _ = fmt.Fprintf(a.Stderr, "amq-keepalive self-upgrade unavailable: %s\n", selfUpgrade.reason)
	}
	if selfUpgrade.startupRefusalReason != "" {
		_, _ = fmt.Fprintf(a.Stderr, "amq-keepalive self-upgrade refused: %s\n", selfUpgrade.startupRefusalReason)
	}

	runOnce := func(emitJSON bool) error {
		results, err := a.superviseOnce(ctx, *registryPath, amq.NewCLI(*amqPath), *self, *wakeTimeout)
		if err != nil {
			return err
		}
		if selfUpgradeHealthyPass(results) {
			if err := selfUpgrade.markSettled(); err != nil {
				return err
			}
		}
		if emitJSON {
			return printJSON(a.Stdout, results)
		}
		return nil
	}
	if *once {
		return runOnce(true)
	}
	for {
		passErr := runOnce(false)
		if notice := selfUpgrade.reloadSettings(); notice != "" {
			_, _ = fmt.Fprintln(a.Stderr, notice)
		}
		if passErr != nil {
			_, _ = fmt.Fprintln(a.Stderr, passErr)
		} else if err := selfUpgrade.maintain(ctx); err != nil {
			_, _ = fmt.Fprintln(a.Stderr, err)
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func selfUpgradeHealthyPass(results []supervisor.Result) bool {
	for _, result := range results {
		if result.Error != nil {
			return false
		}
		// Companion-supervised remote entries are EXCLUDED from results by
		// superviseOnce below — they are out of scope for wake health, and a
		// fabricated placeholder would let one silent entry swing settlement
		// in either direction. Everything left in results is an
		// entry keepalive owns: only a confirmed ensured pass settles the
		// self-upgrade.
		if result.Action != supervisor.ActionEnsured {
			return false
		}
	}
	return true
}

func (a App) superviseOnce(ctx context.Context, registryPath string, wake supervisor.WakeRunner, self string, wakeTimeout time.Duration) ([]supervisor.Result, error) {
	store := registry.New(registryPath)
	var results []supervisor.Result
	err := store.WithRegistrationLockContext(ctx, func() error {
		file, err := store.Load()
		if err != nil {
			return err
		}
		adapters := a.adapterRegistry()
		seedAdapterOwnership(adapters, file.Entries)
		probes := passProbes(file.Entries, adapters)
		conflicts := targetOwnershipConflicts(file, adapters)
		if anyEntryDue(file.Entries, a.now()) {
			mergeOwnershipConflicts(conflicts, physicalOwnershipConflicts(ctx, file, probes))
		}
		results = make([]supervisor.Result, 0, len(file.Entries))
		updates := make([]registry.EntryUpdate, 0, len(file.Entries))
		for _, entry := range file.Entries {
			// Companion-supervised entries (adapter "remote", registered by
			// amq-remote up) own their own supervision loop: up respawns serve
			// with keepalive's backoff constants and forgets its registration
			// on exit. Reconciling them here would probe an adapter this
			// process has no wake adapter for and mark a healthy, live
			// companion as backoff. They are excluded from results entirely,
			// not deferred placeholders: selfUpgradeHealthyPass
			// settles only on ensured results, so a fabricated deferred entry
			// blocked upgrade settlement on every pass whenever a remote
			// companion was registered. Doctor --ops reads them directly.
			if entry.Adapter == companionSupervisedAdapter {
				continue
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				results = append(results, supervisor.Result{Action: supervisor.ActionDeferred, Error: ctxErr})
				continue
			}
			previous := entry
			probe := probes[entry.Adapter]
			if conflictErr, ok := conflicts[entry.ID]; ok {
				probe = fixedProbeError{err: conflictErr}
			}
			reconciler := supervisor.Reconciler{
				Wake:        wake,
				Adapter:     probe,
				Now:         a.Now,
				InjectVia:   self,
				WakeTimeout: wakeTimeout,
			}
			updated, result := reconciler.Reconcile(ctx, entry)
			if result.Action == supervisor.ActionEnsured || result.Action == supervisor.ActionStartFailed {
				if key := lookupOwnershipKey(ctx, probe, updated.Target); key != "" {
					updated.OwnershipKey = key
				}
			}
			if previous != updated {
				updates = append(updates, registry.EntryUpdate{Before: previous, After: updated})
			}
			a.logReconcileTransition(previous, updated, result)
			results = append(results, result)
		}
		_, err = store.UpdateEntries(updates)
		return err
	})
	return results, err
}

func (a App) logReconcileTransition(previous, updated registry.Entry, result supervisor.Result) {
	if previous.State == updated.State &&
		previous.LastError == updated.LastError &&
		previous.LastSupervisorDecision == updated.LastSupervisorDecision {
		return
	}
	w := a.Stderr
	if w == nil {
		w = os.Stderr
	}
	if result.Error != nil {
		_, _ = fmt.Fprintf(w,
			"amq-keepalive reconcile warning: action=%s root=%q agent=%q adapter=%q target=%q failure_count=%d error=%q\n",
			result.Action,
			updated.Root,
			updated.Agent,
			updated.Adapter,
			updated.Target,
			updated.FailureCount,
			result.Error.Error(),
		)
		return
	}
	if updated.State == registry.StateActive {
		_, _ = fmt.Fprintf(w,
			"amq-keepalive reconcile recovered: action=%s root=%q agent=%q adapter=%q target=%q\n",
			result.Action,
			updated.Root,
			updated.Agent,
			updated.Adapter,
			updated.Target,
		)
	}
}

func (a App) rateLimitedAdapterLogf(state *adapterLogState) func(string, ...any) {
	return func(format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		now := time.Now()
		state.mu.Lock()
		if state.last == nil {
			state.last = map[string]time.Time{}
		}
		if previous, ok := state.last[message]; ok && now.Sub(previous) < adapterLogInterval {
			state.mu.Unlock()
			return
		}
		state.last[message] = now
		state.mu.Unlock()

		w := a.Stderr
		if w == nil {
			w = os.Stderr
		}
		_, _ = fmt.Fprintf(w, "amq-keepalive adapter: %s\n", message)
	}
}

func anyEntryDue(entries []registry.Entry, now time.Time) bool {
	for _, entry := range entries {
		if !entry.NextHealthCheck.IsZero() && now.Before(entry.NextHealthCheck) {
			continue
		}
		if !entry.BackoffUntil.IsZero() && now.Before(entry.BackoffUntil) {
			continue
		}
		return true
	}
	return false
}
