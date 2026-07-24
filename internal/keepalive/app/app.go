package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/launchd"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
)

type App struct {
	Stdout   io.Writer
	Stderr   io.Writer
	Adapters *adapter.Registry

	adapterLogState *adapterLogState
}

const adapterLogInterval = 5 * time.Minute

type adapterLogState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// wakeRetirer stops an identity-confirmed live inject-via wake (or removes its
// exactly-bound proven-stale lock) whose saved target matches exactly. The
// concrete implementation is amq.CLI; the interface keeps the app callers
// testable with the same fake amq executables used across this package.
type wakeRetirer interface {
	RetireWake(ctx context.Context, req amq.RetireWakeRequest) (amq.RetireWakeResult, error)
}

func (a App) Run(ctx context.Context, args []string) int {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.adapterLogState == nil {
		a.adapterLogState = &adapterLogState{last: map[string]time.Time{}}
	}
	if len(args) == 0 {
		a.usage(a.Stderr)
		return 2
	}

	var err error
	switch args[0] {
	case "-h", "--help", "help":
		a.usage(a.Stdout)
		return 0
	case "attach":
		err = a.attach(ctx, args[1:])
	case "reattach":
		err = a.reattach(ctx, args[1:])
	case "supervise":
		err = a.supervise(ctx, args[1:])
	case "inject":
		err = a.inject(ctx, args[1:])
	case "doctor":
		err = a.doctor(args[1:])
	case "gc":
		err = a.gc(ctx, args[1:])
	case "retire-session":
		err = a.retireSession(ctx, args[1:])
	case "forget":
		err = a.forget(ctx, args[1:])
	case "install-launchd":
		err = a.installLaunchd(ctx, args[1:])
	case "install-hook":
		err = a.installHook(args[1:])
	case "uninstall":
		err = a.uninstallLaunchd(ctx, args[1:])
	default:
		_, _ = fmt.Fprintf(a.Stderr, "unknown command %q\n", args[0])
		a.usage(a.Stderr)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(a.Stderr, err)
		return 1
	}
	return 0
}

type registerOptions struct {
	RegistryPath   string
	AdapterName    string
	Target         string
	Root           string
	BaseRoot       string
	SessionName    string
	Me             string
	AMQPath        string
	Self           string
	WakeTimeout    time.Duration
	NoStart        bool
	Replace        bool
	RetireDetached bool
}

type registerResult struct {
	Entry          registry.Entry   `json:"entry"`
	RemovedEntries []registry.Entry `json:"removed_entries,omitempty"`
}

func (a App) attach(ctx context.Context, args []string) error {
	return a.register(ctx, args, false)
}

func (a App) reattach(ctx context.Context, args []string) error {
	return a.register(ctx, args, true)
}

func (a App) register(ctx context.Context, args []string, replace bool) error {
	commandName := "attach"
	if replace {
		commandName = "reattach"
	}
	fs := flag.NewFlagSet(commandName, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	adapterName := fs.String("adapter", "file", "adapter name")
	target := fs.String("target", "", "adapter target")
	root := fs.String("root", "", "AMQ root")
	baseRoot := fs.String("base-root", "", "AMQ base root")
	sessionName := fs.String("session", "", "AMQ session name")
	me := fs.String("me", "", "AMQ agent handle")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", executablePath(), "amq-keepalive executable path for --inject-via")
	wakeTimeout := fs.Duration("wake-ready-timeout", 10*time.Second, "maximum time to wait for amq wake readiness")
	noStart := fs.Bool("no-start", false, "register without starting/reconciling wake")
	retireDetached := fs.Bool("retire-detached", false, "on a recreated terminal, retire the proven-absent previous wake via amq wake retire before converging on the exact new target")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return a.registerWithOptions(ctx, registerOptions{
		RegistryPath:   *registryPath,
		AdapterName:    *adapterName,
		Target:         *target,
		Root:           *root,
		BaseRoot:       *baseRoot,
		SessionName:    *sessionName,
		Me:             *me,
		AMQPath:        *amqPath,
		Self:           *self,
		WakeTimeout:    *wakeTimeout,
		NoStart:        *noStart,
		Replace:        replace,
		RetireDetached: *retireDetached,
	})
}

func (a App) registerWithOptions(ctx context.Context, opts registerOptions) error {
	envCLI := amq.NewCLI(opts.AMQPath)
	if opts.Root == "" || opts.Me == "" || opts.BaseRoot == "" || opts.SessionName == "" {
		env, err := envCLI.Env(ctx)
		if err != nil && (opts.Root == "" || opts.Me == "") {
			return err
		}
		if opts.Root == "" {
			opts.Root = env.Root
		}
		if opts.BaseRoot == "" {
			opts.BaseRoot = env.BaseRoot
		}
		if opts.SessionName == "" {
			opts.SessionName = env.SessionName
		}
		if opts.Me == "" {
			opts.Me = env.Me
		}
	}
	opts.Root, opts.BaseRoot = normalizeAMQPaths(opts.Root, opts.BaseRoot, opts.SessionName)

	adapters := a.adapterRegistry()
	selected, err := adapters.Get(opts.AdapterName)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		discoverer, ok := selected.(adapter.Discoverer)
		if !ok {
			return errors.New("--target is required")
		}
		discovered, err := discoverer.Discover(ctx)
		if err != nil {
			return err
		}
		opts.Target = discovered
	}
	if normalizer, ok := selected.(adapter.TargetNormalizer); ok {
		normalized, err := normalizer.NormalizeTarget(opts.Target)
		if err != nil {
			return err
		}
		opts.Target = normalized
	}
	ownershipContext := registrationOwnershipContext(ctx, selected, opts.Target)
	store := registry.New(opts.RegistryPath)
	next := registry.Entry{
		ID:          registry.EntryID(opts.Root, opts.Me, opts.AdapterName, opts.Target),
		Root:        opts.Root,
		BaseRoot:    opts.BaseRoot,
		SessionName: opts.SessionName,
		Agent:       opts.Me,
		Adapter:     opts.AdapterName,
		Target:      opts.Target,
		State:       registry.StateAttached,
	}
	reconciler := supervisor.Reconciler{
		Wake:        envCLI,
		Adapter:     selected,
		InjectVia:   opts.Self,
		WakeTimeout: opts.WakeTimeout,
	}

	var entry registry.Entry
	var removed []registry.Entry
	err = store.WithRegistrationLockContext(ctx, func() error {
		// Refresh existence and physical ownership only after acquiring the
		// cross-process registration lease. The lease remains held through wake
		// readiness and registry commit, so a racing claimant observes this
		// transaction's committed owner before it can touch AMQ.
		inventory, err := registrationTargetInventory(ctx, selected, next.Target, ownershipContext)
		if err != nil {
			return err
		}
		file, err := store.Load()
		if err != nil {
			return err
		}
		if inventory != nil {
			if err := checkPhysicalTargetAvailable(file, selected, inventory, next, opts.Replace); err != nil {
				return err
			}
			reconciler.Adapter = targetInventoryProbe{inventory: inventory}
		}

		if opts.Replace {
			if err := store.CheckTargetAvailable(next, true); err != nil {
				return err
			}
			// Persist an inactive reservation before touching AMQ. If this process
			// crashes after readiness, the supervisor can recover the registered
			// candidate; there is no post-readiness commit window which could leave
			// a live wake unregistered.
			entry, removed, err = store.ReplaceSessionAdapter(next)
			if err != nil || opts.NoStart {
				return err
			}
			next = entry
			wakeReady := false
			if !opts.NoStart {
				if opts.RetireDetached {
					var recoverErr error
					next, wakeReady, recoverErr = recoverDetachedRegistration(ctx, removed, adapters, reconciler, envCLI, next)
					if recoverErr != nil {
						return resolveRegistrationReadinessFailure(store, entry, next, removed, recoverErr)
					}
				}
				if !wakeReady {
					updated, result := reconciler.StartFresh(ctx, next)
					if result.Error != nil {
						return resolveRegistrationReadinessFailure(store, entry, updated, removed, result.Error)
					}
					next = updated
				}
			}
			if err := store.UpdateEntry(next); err != nil {
				return fmt.Errorf("wake is ready and its attached registry reservation remains recoverable, but marking it active failed: %w", err)
			}
			entry = next
			return nil
		}

		entry, err = store.Upsert(next)
		if err != nil || opts.NoStart {
			return err
		}
		updated, result := reconciler.Reconcile(ctx, entry)
		if updateErr := store.UpdateEntry(updated); updateErr != nil {
			return updateErr
		}
		entry = updated
		if result.Error != nil && result.Action != supervisor.ActionDetached {
			return result.Error
		}
		return nil
	})
	if err != nil {
		return err
	}
	if opts.Replace {
		return printJSON(a.Stdout, registerResult{Entry: entry, RemovedEntries: removed})
	}
	return printJSON(a.Stdout, entry)
}

func resolveRegistrationReadinessFailure(
	store *registry.Store,
	reservation registry.Entry,
	candidate registry.Entry,
	removed []registry.Entry,
	readinessErr error,
) error {
	if errors.Is(readinessErr, amq.ErrWakeReadinessUncertain) {
		if candidate != reservation {
			if updateErr := store.UpdateEntry(candidate); updateErr != nil {
				return errors.Join(
					readinessErr,
					fmt.Errorf("wake readiness is uncertain and the attached reservation remains, but recording its retry state failed: %w", updateErr),
				)
			}
		}
		return fmt.Errorf("wake readiness is uncertain; the attached registry reservation was preserved for supervisor convergence: %w", readinessErr)
	}
	restored, restoreErr := store.RestoreSessionAdapterIfUnchanged(reservation, removed)
	if restoreErr != nil {
		return errors.Join(readinessErr, fmt.Errorf("restore previous registry entries after wake readiness failure: %w", restoreErr))
	}
	if !restored {
		return errors.Join(readinessErr, errors.New("registry reservation changed before previous entries could be restored; the attached reservation was preserved for supervisor recovery"))
	}
	return readinessErr
}

type targetInventoryProbe struct {
	inventory adapter.TargetInventory
}

func (p targetInventoryProbe) Probe(_ context.Context, target string) error {
	return p.inventory.Probe(target)
}

func registrationOwnershipContext(ctx context.Context, selected adapter.Adapter, target string) adapter.OwnershipContext {
	if selected.Name() != "cmux" {
		return adapter.OwnershipContext{}
	}
	discoverer, ok := selected.(adapter.Discoverer)
	if !ok {
		return adapter.OwnershipContext{}
	}
	discovered, err := discoverer.Discover(ctx)
	if err != nil {
		return adapter.OwnershipContext{}
	}
	if normalizer, ok := selected.(adapter.TargetNormalizer); ok {
		discovered, err = normalizer.NormalizeTarget(discovered)
		if err != nil {
			return adapter.OwnershipContext{}
		}
	}
	if discovered != target {
		return adapter.OwnershipContext{}
	}
	return adapter.OwnershipContext{TrustedTarget: target}
}

func registrationTargetInventory(
	ctx context.Context,
	selected adapter.Adapter,
	target string,
	ownershipContext adapter.OwnershipContext,
) (adapter.TargetInventory, error) {
	provider, ok := selected.(adapter.InventoryProvider)
	if !ok {
		return nil, selected.Probe(ctx, target)
	}
	// Only a target independently matched to the current CMUX_SURFACE_ID is
	// live by construction. An explicit --target without that match receives
	// the zero context and remains fail-closed when aliases are ambiguous.
	inventory, err := provider.Inventory(ctx, ownershipContext)
	if err != nil {
		return nil, err
	}
	if inventory == nil {
		return nil, errors.New("adapter returned a nil target inventory")
	}
	if err := inventory.Probe(target); err != nil {
		return nil, err
	}
	return inventory, nil
}

func checkPhysicalTargetAvailable(
	file registry.File,
	selected adapter.Adapter,
	inventory adapter.TargetInventory,
	candidate registry.Entry,
	ignoreSameRootAgent bool,
) error {
	candidateKey, err := inventory.OwnershipKey(candidate.Target)
	if err != nil {
		return fmt.Errorf("resolve requested physical target ownership: %w", err)
	}
	for _, existing := range file.Entries {
		if existing.Adapter != candidate.Adapter || existing.ID == candidate.ID {
			continue
		}
		if ignoreSameRootAgent && existing.Root == candidate.Root && existing.Agent == candidate.Agent {
			continue
		}
		target, err := normalizedTarget(selected, existing.Target)
		if err != nil {
			return fmt.Errorf("resolve registered target ownership for %s@%s: %w", existing.Agent, existing.Root, err)
		}
		existingKey, err := inventory.OwnershipKey(target)
		if errors.Is(err, adapter.ErrTargetNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve registered physical target ownership for %s@%s: %w", existing.Agent, existing.Root, err)
		}
		if existingKey == candidateKey {
			return fmt.Errorf(
				"%w: adapter=%q physical_identity=%q requested_target=%q requested_by=%s@%s existing_target=%q existing_owner=%s@%s existing_id=%s",
				registry.ErrTargetOwned, candidate.Adapter, candidateKey, candidate.Target, candidate.Agent, candidate.Root,
				existing.Target, existing.Agent, existing.Root, existing.ID,
			)
		}
	}
	return nil
}

// recoverDetachedRegistration is the narrow recovery path for a recreated
// terminal. It never retargets a live wake: the previously registered adapter
// target must be independently proven absent. It first asks AMQ's atomic wake
// start path to converge on the new exact target, which handles an already
// absent lock without requiring retirement. If a live old wake blocks that
// start, it asks AMQ to retire that exact previous inject-via identity and then
// retries the start once. AMQ retires only an identity-confirmed live wake (or
// removes its exactly-bound proven-stale lock); if it refuses, this path fails
// without touching the live wake and the previous registry row is restored.
func recoverDetachedRegistration(
	ctx context.Context,
	previousEntries []registry.Entry,
	adapters adapter.Registry,
	reconciler supervisor.Reconciler,
	retirer wakeRetirer,
	next registry.Entry,
) (registry.Entry, bool, error) {
	matches := make([]registry.Entry, 0, 1)
	for _, entry := range previousEntries {
		if entry.Root == next.Root && entry.Agent == next.Agent {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return next, false, nil
	}
	if len(matches) != 1 {
		return next, false, fmt.Errorf("refusing detached wake recovery for %s at %s: expected one registry entry, found %d", next.Agent, next.Root, len(matches))
	}
	previous := matches[0]
	if previous.Adapter == next.Adapter && previous.Target == next.Target {
		return next, false, nil
	}
	previousAdapter, err := adapters.Get(previous.Adapter)
	if err != nil {
		return next, false, fmt.Errorf("load previous adapter %s: %w", previous.Adapter, err)
	}
	if normalizer, ok := previousAdapter.(adapter.TargetNormalizer); ok {
		previous.Target, err = normalizer.NormalizeTarget(previous.Target)
		if err != nil {
			return next, false, fmt.Errorf("normalize previous target for %s: %w", previous.Agent, err)
		}
	}
	probeErr := previousAdapter.Probe(ctx, previous.Target)
	if probeErr == nil {
		return next, false, nil
	}
	if !errors.Is(probeErr, adapter.ErrTargetNotFound) {
		return next, false, fmt.Errorf("refusing detached wake recovery for %s because target absence is not proven: %w", previous.Agent, probeErr)
	}

	updated, initialStart := reconciler.StartFresh(ctx, next)
	if initialStart.Error == nil {
		return updated, true, nil
	}
	if errors.Is(initialStart.Error, amq.ErrWakeReadinessUncertain) {
		return updated, false, fmt.Errorf("recover detached %s wake has uncertain readiness: %w", previous.Agent, initialStart.Error)
	}

	// The previous adapter target is proven absent, but a wake still bound to it
	// may hold the lock and block the exact-target start. Ask AMQ to retire that
	// exact previous inject-via identity. AMQ retires only an identity-confirmed
	// live wake with an unchanged matching saved target, or removes its
	// exactly-bound proven-stale lock; it refuses anything else.
	if _, retireErr := retirer.RetireWake(ctx, amq.RetireWakeRequest{
		Root:      next.Root,
		Me:        next.Agent,
		InjectVia: reconciler.InjectVia,
		Adapter:   previous.Adapter,
		Target:    previous.Target,
	}); retireErr != nil {
		return next, false, fmt.Errorf(
			"recover detached %s wake: exact-target start failed (%v) and retiring the previous wake was not confirmed: %w",
			previous.Agent, initialStart.Error, retireErr,
		)
	}

	retried, retryStart := reconciler.StartFresh(ctx, next)
	if retryStart.Error == nil {
		return retried, true, nil
	}
	if errors.Is(retryStart.Error, amq.ErrWakeReadinessUncertain) {
		return retried, false, fmt.Errorf("recover detached %s wake has uncertain readiness after retiring the previous wake: %w", previous.Agent, retryStart.Error)
	}
	return next, false, fmt.Errorf(
		"recover detached %s wake: exact-target start still failed after retiring the previous wake: %w",
		previous.Agent, retryStart.Error,
	)
}

func (a App) supervise(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", executablePath(), "amq-keepalive executable path for --inject-via")
	once := fs.Bool("once", false, "run one supervisor pass")
	interval := fs.Duration("interval", time.Minute, "supervisor interval")
	wakeTimeout := fs.Duration("wake-ready-timeout", 10*time.Second, "maximum time to wait for amq wake readiness")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *interval <= 0 {
		return errors.New("--interval must be greater than zero")
	}

	runOnce := func(emitJSON bool) error {
		results, err := a.superviseOnce(ctx, *registryPath, amq.NewCLI(*amqPath), *self, *wakeTimeout)
		if err != nil {
			return err
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
		if err := runOnce(false); err != nil {
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

func (a App) superviseOnce(ctx context.Context, registryPath string, wake supervisor.WakeRunner, self string, wakeTimeout time.Duration) ([]supervisor.Result, error) {
	store := registry.New(registryPath)
	var results []supervisor.Result
	err := store.WithRegistrationLockContext(ctx, func() error {
		file, err := store.Load()
		if err != nil {
			return err
		}
		adapters := a.adapterRegistry()
		probes := passProbes(file.Entries, adapters)
		conflicts := targetOwnershipConflicts(file, adapters)
		if anyEntryDue(file.Entries, time.Now().UTC()) {
			mergeOwnershipConflicts(conflicts, physicalOwnershipConflicts(ctx, file, probes))
		}
		results = make([]supervisor.Result, 0, len(file.Entries))
		updates := make([]registry.EntryUpdate, 0, len(file.Entries))
		for _, entry := range file.Entries {
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
				InjectVia:   self,
				WakeTimeout: wakeTimeout,
			}
			updated, result := reconciler.Reconcile(ctx, entry)
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

func (a App) adapterRegistry() adapter.Registry {
	if a.Adapters != nil {
		return *a.Adapters
	}
	state := a.adapterLogState
	if state == nil {
		state = &adapterLogState{last: map[string]time.Time{}}
	}
	return adapter.DefaultRegistryWithLogf(a.rateLimitedAdapterLogf(state))
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

func (a App) inject(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: amq-keepalive inject <adapter> <target> <payload>")
	}
	adapters := a.adapterRegistry()
	selected, err := adapters.Get(args[0])
	if err != nil {
		return err
	}
	return selected.Inject(ctx, args[1], args[2])
}

func (a App) doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store := registry.New(*registryPath)
	file, err := store.Load()
	if err != nil {
		return err
	}
	return printJSON(a.Stdout, file)
}

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

// gc classifies proven-detached registry entries. With --apply it retires each
// candidate's exact previous inject-via wake via amq wake retire and forgets
// only the entries AMQ positively confirmed retired; any refusal leaves the
// entry in place for the next pass and surfaces an error.
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
	if *minDetachedAge < 0 {
		return errors.New("--min-detached-age must not be negative")
	}

	var retirer wakeRetirer = amq.NewCLI(*amqPath)
	store := registry.New(*registryPath)
	file, err := store.Load()
	if err != nil {
		return err
	}
	adapters := a.adapterRegistry()
	probes := passProbes(file.Entries, adapters)
	conflicts := targetOwnershipConflicts(file, adapters)
	mergeOwnershipConflicts(conflicts, physicalOwnershipConflicts(ctx, file, probes))
	now := time.Now().UTC()
	result := gcResult{Applied: *apply, MinDetachedAge: minDetachedAge.String()}
	var applyErrs []error
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
			Root:      entry.Root,
			Me:        entry.Agent,
			InjectVia: *self,
			Adapter:   entry.Adapter,
			Target:    target,
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
		for i := range entries {
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
		var retirer wakeRetirer = amq.NewCLI(*amqPath)
		var retireErrs []error
		for i := range entries {
			if _, retireErr := retirer.RetireWake(ctx, amq.RetireWakeRequest{
				Root:      entries[i].Root,
				Me:        entries[i].Agent,
				InjectVia: *self,
				Adapter:   entries[i].Adapter,
				Target:    targets[i],
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
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
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

func (a App) installLaunchd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install-launchd", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	label := fs.String("label", launchd.DefaultLabel, "launchd label")
	plistPath := fs.String("plist", "", "plist path")
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", executablePath(), "amq-keepalive executable path")
	interval := fs.Duration("interval", time.Minute, "supervisor interval")
	noLoad := fs.Bool("no-load", false, "write plist without loading it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := launchd.Options{
		Label:        *label,
		PlistPath:    *plistPath,
		BinaryPath:   *self,
		RegistryPath: *registryPath,
		AMQPath:      *amqPath,
		Interval:     *interval,
		Load:         !*noLoad,
	}
	normalized, err := launchd.NormalizeOptions(opts)
	if err != nil {
		return err
	}
	if err := launchd.Install(ctx, normalized); err != nil {
		return err
	}
	return printJSON(a.Stdout, map[string]any{
		"label":      normalized.Label,
		"plist":      normalized.PlistPath,
		"loaded":     normalized.Load,
		"supervisor": normalized.BinaryPath,
	})
}

func (a App) installHook(args []string) error {
	fs := flag.NewFlagSet("install-hook", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	agent := fs.String("agent", hookinstall.AgentBoth, "agent config to update: claude, codex, or both")
	scriptPath := fs.String("script", "", "installed hook script path")
	binaryPath := fs.String("bin", executablePath(), "amq-keepalive binary path")
	claudeConfig := fs.String("claude-config", "", "Claude settings.json path")
	codexConfig := fs.String("codex-config", "", "Codex hooks.json path")
	timeout := fs.Duration("timeout", hookinstall.DefaultTimeout, "self-timeout for reattach work inside the hook")
	dryRun := fs.Bool("dry-run", false, "print install plan without writing files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := hookinstall.Install(hookinstall.Options{
		Agent:        *agent,
		ScriptPath:   *scriptPath,
		BinaryPath:   *binaryPath,
		ClaudeConfig: *claudeConfig,
		CodexConfig:  *codexConfig,
		Timeout:      *timeout,
		DryRun:       *dryRun,
	})
	if err != nil {
		return err
	}
	return printJSON(a.Stdout, result)
}

func (a App) uninstallLaunchd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	label := fs.String("label", launchd.DefaultLabel, "launchd label")
	plistPath := fs.String("plist", "", "plist path")
	noUnload := fs.Bool("no-unload", false, "remove plist without bootout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := launchd.Uninstall(ctx, *label, *plistPath, !*noUnload); err != nil {
		return err
	}
	return printJSON(a.Stdout, map[string]any{"label": *label, "removed": true, "unloaded": !*noUnload})
}

func (a App) usage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "usage: amq-keepalive <attach|reattach|supervise|inject|doctor|gc|retire-session|forget|install-launchd|install-hook|uninstall> [options]")
}

func mustDefaultRegistryPath() string {
	path, err := registry.DefaultPath()
	if err != nil {
		return ""
	}
	return path
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return path
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func normalizeAMQPaths(root, baseRoot, sessionName string) (string, string) {
	root = strings.TrimSpace(root)
	baseRoot = strings.TrimSpace(baseRoot)
	sessionName = strings.TrimSpace(sessionName)

	if baseRoot != "" && !filepath.IsAbs(baseRoot) {
		if abs, err := filepath.Abs(baseRoot); err == nil {
			baseRoot = abs
		}
	}
	if root == "" || filepath.IsAbs(root) {
		return root, baseRoot
	}
	if baseRoot != "" && filepath.IsAbs(baseRoot) {
		if sessionName != "" && filepath.Base(root) == sessionName {
			return filepath.Join(baseRoot, sessionName), baseRoot
		}
		if filepath.Base(root) == filepath.Base(baseRoot) {
			return baseRoot, baseRoot
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs, baseRoot
	}
	return root, baseRoot
}
