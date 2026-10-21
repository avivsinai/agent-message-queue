package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
)

// afterReattachRegistrationLockHeldForTest runs after reattach/attach holds the
// registration lease and before the transaction body. Tests pause here so gc
// cannot Load until the lock is released.
var afterReattachRegistrationLockHeldForTest func()

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
	MinCapability  adapter.Capability
}

type registerResult struct {
	Entry          registry.Entry   `json:"entry"`
	RemovedEntries []registry.Entry `json:"removed_entries,omitempty"`
}

// parseMinCapability translates the --min-delivery / --min-session /
// --accept-requires-human flags into a Capability minimum. A zero value (the
// default) preserves today's behavior: only the strongest unattended seats
// satisfy it, so a human-required prefill seat like claude-desktop is refused
// unless the caller explicitly opts into it.
func parseMinCapability(delivery, session string, acceptRequiresHuman bool) (adapter.Capability, error) {
	d, err := parseDeliveryFlag(delivery)
	if err != nil {
		return adapter.Capability{}, err
	}
	s, err := parseSessionFlag(session)
	if err != nil {
		return adapter.Capability{}, err
	}
	return adapter.Capability{
		Delivery:      d,
		Session:       s,
		RequiresHuman: acceptRequiresHuman,
	}, nil
}

func parseDeliveryFlag(s string) (adapter.Delivery, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return adapter.DeliveryNone, nil
	case "prefilled":
		return adapter.DeliveryPrefilled, nil
	case "submitted":
		return adapter.DeliverySubmitted, nil
	default:
		return 0, fmt.Errorf("invalid --min-delivery %q: want none|prefilled|submitted", s)
	}
}

func parseSessionFlag(s string) (adapter.SessionScope, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return adapter.SessionNone, nil
	case "new":
		return adapter.SessionNew, nil
	case "existing-exact":
		return adapter.SessionExistingExact, nil
	default:
		return 0, fmt.Errorf("invalid --min-session %q: want none|new|existing-exact", s)
	}
}

func minCapabilityLabel(d adapter.Delivery) string {
	switch d {
	case adapter.DeliveryPrefilled:
		return "prefilled"
	case adapter.DeliverySubmitted:
		return "submitted"
	default:
		return "none"
	}
}

func sessionLabel(s adapter.SessionScope) string {
	switch s {
	case adapter.SessionNew:
		return "new"
	case adapter.SessionExistingExact:
		return "existing-exact"
	default:
		return "none"
	}
}

func activationLabel(a adapter.Activation) string {
	switch a {
	case adapter.ActivationLaunch:
		return "launch"
	case adapter.ActivationForeground:
		return "foreground"
	default:
		return "none"
	}
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
	minDelivery := fs.String("min-delivery", "none", "minimum delivery capability required from the adapter: none|prefilled|submitted")
	minSession := fs.String("min-session", "none", "minimum session capability required from the adapter: none|new|existing-exact")
	acceptRequiresHuman := fs.Bool("accept-requires-human", false, "accept an adapter that requires a human to send the prompt (prefill-only seats)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	minCapability, err := parseMinCapability(*minDelivery, *minSession, *acceptRequiresHuman)
	if err != nil {
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
		MinCapability:  minCapability,
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
	canonicalRoot, err := registry.CanonicalRoot(opts.Root)
	if err != nil {
		return fmt.Errorf("canonicalize AMQ root: %w", err)
	}
	opts.Root = canonicalRoot

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
	// Capability gate runs AFTER target normalization so a target-aware adapter
	// can declare a per-target vector (e.g. new vs existing-exact). A target
	// normalization error has already failed above. Prefer
	// CapabilityForTarget(target) when implemented; else Capability(); else
	// UnknownCapability() (weakest + requires a human). An adapter that forgets
	// to declare any Capability is refused unless the caller explicitly
	// tolerates a human-required seat — fail-closed, not fail-open.
	var declared adapter.Capability
	if td, ok := selected.(adapter.TargetCapabilityDeclarer); ok {
		c, err := td.CapabilityForTarget(opts.Target)
		if err != nil {
			return fmt.Errorf("adapter %s target %q: %w", opts.AdapterName, opts.Target, err)
		}
		declared = c
	} else if d, ok := selected.(adapter.CapabilityDeclarer); ok {
		declared = d.Capability()
	} else {
		declared = adapter.UnknownCapability()
	}
	if !declared.Satisfies(opts.MinCapability) {
		return fmt.Errorf("adapter %s does not satisfy the requested capability (declared: activation=%s delivery=%s session=%s requires_human=%t; min: activation=%s delivery=%s session=%s requires_human=%t); refusing without substitution", opts.AdapterName, activationLabel(declared.Activation), minCapabilityLabel(declared.Delivery), sessionLabel(declared.Session), declared.RequiresHuman, activationLabel(opts.MinCapability.Activation), minCapabilityLabel(opts.MinCapability.Delivery), sessionLabel(opts.MinCapability.Session), opts.MinCapability.RequiresHuman)
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
		Wake:        a.wakeRunner(envCLI),
		Adapter:     selected,
		Now:         a.Now,
		InjectVia:   opts.Self,
		WakeTimeout: opts.WakeTimeout,
	}

	var entry registry.Entry
	var removed []registry.Entry
	err = store.WithRegistrationLockContext(ctx, func() error {
		if afterReattachRegistrationLockHeldForTest != nil {
			afterReattachRegistrationLockHeldForTest()
		}
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
			if key, keyErr := inventory.OwnershipKey(next.Target); keyErr == nil {
				next.OwnershipKey = key
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
		if ignoreSameRootAgent {
			sameOwner, err := sameCanonicalRootAgent(existing, candidate)
			if err != nil {
				return fmt.Errorf("canonicalize registered AMQ root for %s@%s: %w", existing.Agent, existing.Root, err)
			}
			if sameOwner {
				continue
			}
		}
		target, err := normalizedTarget(selected, existing.Target)
		if err != nil {
			return fmt.Errorf("resolve registered target ownership for %s@%s: %w", existing.Agent, existing.Root, err)
		}
		existingKey, err := inventory.OwnershipKey(target)
		if errors.Is(err, adapter.ErrTargetNotFound) {
			continue
		}
		if errors.Is(err, adapter.ErrTargetDegraded) {
			degradedKey, ok := adapter.CmuxDegradedOwnershipKey(inventory, err)
			if !ok {
				return fmt.Errorf("resolve registered physical target ownership for %s@%s: %w", existing.Agent, existing.Root, err)
			}
			if degradedKey != candidateKey {
				// Preserve the uncertain registration for supervisor retry, but a
				// physical key proven different by the same inventory cannot own the
				// candidate's device.
				continue
			}
			existingKey = degradedKey
			err = nil
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

func sameCanonicalRootAgent(left, right registry.Entry) (bool, error) {
	if left.Agent != right.Agent {
		return false, nil
	}
	leftRoot, err := registry.CanonicalRoot(left.Root)
	if err != nil {
		return false, err
	}
	rightRoot, err := registry.CanonicalRoot(right.Root)
	if err != nil {
		return false, err
	}
	return leftRoot == rightRoot, nil
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
		sameOwner, err := sameCanonicalRootAgent(entry, next)
		if err != nil {
			return next, false, fmt.Errorf("canonicalize previous AMQ root for detached wake recovery: %w", err)
		}
		if sameOwner {
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

	generation, err := observeWakeGeneration(ctx, retirer, next.Root, next.Agent)
	if err != nil {
		return next, false, fmt.Errorf("recover detached %s wake: %w", previous.Agent, err)
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
		Root:       next.Root,
		Me:         next.Agent,
		InjectVia:  reconciler.InjectVia,
		Adapter:    previous.Adapter,
		Target:     previous.Target,
		Generation: generation,
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
