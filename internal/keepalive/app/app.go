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
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/launchd"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer
}

func (a App) Run(ctx context.Context, args []string) int {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
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
	case "retire-session":
		err = a.retireSession(ctx, args[1:])
	case "forget":
		err = a.forget(args[1:])
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
	RegistryPath string
	AdapterName  string
	Target       string
	Root         string
	BaseRoot     string
	SessionName  string
	Me           string
	AMQPath      string
	Self         string
	WakeTimeout  time.Duration
	NoStart      bool
	Replace      bool
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	return a.registerWithOptions(ctx, registerOptions{
		RegistryPath: *registryPath,
		AdapterName:  *adapterName,
		Target:       *target,
		Root:         *root,
		BaseRoot:     *baseRoot,
		SessionName:  *sessionName,
		Me:           *me,
		AMQPath:      *amqPath,
		Self:         *self,
		WakeTimeout:  *wakeTimeout,
		NoStart:      *noStart,
		Replace:      replace,
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

	adapters := adapter.DefaultRegistry()
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
	if err := selected.Probe(ctx, opts.Target); err != nil {
		return err
	}

	store := registry.New(opts.RegistryPath)
	next := registry.Entry{
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

	if opts.Replace {
		if !opts.NoStart {
			updated, result := reconciler.StartFresh(ctx, next)
			if result.Error != nil {
				return result.Error
			}
			next = updated
		}
		entry, removed, err := store.ReplaceSessionAdapter(next)
		if err != nil {
			return err
		}
		return printJSON(a.Stdout, registerResult{Entry: entry, RemovedEntries: removed})
	}

	entry, err := store.Upsert(next)
	if err != nil {
		return err
	}
	if !opts.NoStart {
		updated, result := reconciler.Reconcile(ctx, entry)
		if updateErr := store.UpdateEntry(updated); updateErr != nil {
			return updateErr
		}
		entry = updated
		if result.Error != nil && result.Action != supervisor.ActionDetached {
			return result.Error
		}
	}
	return printJSON(a.Stdout, entry)
}

func (a App) supervise(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", executablePath(), "amq-keepalive executable path for --inject-via")
	once := fs.Bool("once", false, "run one supervisor pass")
	interval := fs.Duration("interval", 10*time.Second, "supervisor interval")
	wakeTimeout := fs.Duration("wake-ready-timeout", 10*time.Second, "maximum time to wait for amq wake readiness")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runOnce := func() error {
		return a.superviseOnce(ctx, *registryPath, amq.NewCLI(*amqPath), *self, *wakeTimeout)
	}
	if *once {
		return runOnce()
	}
	for {
		if err := runOnce(); err != nil {
			_, _ = fmt.Fprintln(a.Stderr, err)
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (a App) superviseOnce(ctx context.Context, registryPath string, wake supervisor.WakeRunner, self string, wakeTimeout time.Duration) error {
	store := registry.New(registryPath)
	file, err := store.Load()
	if err != nil {
		return err
	}
	adapters := adapter.DefaultRegistry()
	results := make([]supervisor.Result, 0, len(file.Entries))
	for _, entry := range file.Entries {
		previous := entry
		selected, err := adapters.Get(entry.Adapter)
		if err != nil {
			entry.LastError = err.Error()
			entry.LastSupervisorDecision = supervisor.ActionBackoff
			entry.State = registry.StateAttached
			if updateErr := store.UpdateEntry(entry); updateErr != nil {
				return updateErr
			}
			result := supervisor.Result{Action: supervisor.ActionBackoff, Error: err}
			a.warnReconcileFailure(previous, entry, result)
			results = append(results, result)
			continue
		}
		reconciler := supervisor.Reconciler{
			Wake:        wake,
			Adapter:     selected,
			InjectVia:   self,
			WakeTimeout: wakeTimeout,
		}
		updated, result := reconciler.Reconcile(ctx, entry)
		if err := store.UpdateEntry(updated); err != nil {
			return err
		}
		a.warnReconcileFailure(previous, updated, result)
		results = append(results, result)
	}
	return printJSON(a.Stdout, results)
}

func (a App) warnReconcileFailure(previous, updated registry.Entry, result supervisor.Result) {
	if result.Error == nil {
		return
	}
	switch result.Action {
	case supervisor.ActionBackoff, supervisor.ActionDetached, supervisor.ActionStartFailed:
	default:
		return
	}
	if previous.State == updated.State &&
		previous.LastError == updated.LastError &&
		previous.LastSupervisorDecision == updated.LastSupervisorDecision &&
		previous.FailureCount == updated.FailureCount {
		return
	}
	w := a.Stderr
	if w == nil {
		w = os.Stderr
	}
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
}

func (a App) inject(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: amq-keepalive inject <adapter> <target> <payload>")
	}
	adapters := adapter.DefaultRegistry()
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

func (a App) forget(args []string) error {
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
	removed, err := store.Forget(*id)
	if err != nil {
		return err
	}
	return printJSON(a.Stdout, map[string]any{"removed": removed})
}

type retiredSessionEntry struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Target string `json:"target"`
	Status string `json:"status"`
	PID    int    `json:"pid,omitempty"`
}

type retireSessionResult struct {
	Root    string                `json:"root"`
	Adapter string                `json:"adapter"`
	Entries []retiredSessionEntry `json:"entries"`
}

func (a App) retireSession(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("retire-session", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	rootFlag := fs.String("root", "", "exact AMQ session root")
	adapterName := fs.String("adapter", "cmux", "adapter name")
	agentsFlag := fs.String("agents", "codex,claude", "comma-separated required agent handles")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", executablePath(), "amq-keepalive executable path used by the wake")
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

	adapters := adapter.DefaultRegistry()
	selected, err := adapters.Get(*adapterName)
	if err != nil {
		return err
	}
	for i := range entries {
		entry := &entries[i]
		if normalizer, ok := selected.(adapter.TargetNormalizer); ok {
			normalized, normalizeErr := normalizer.NormalizeTarget(entry.Target)
			if normalizeErr != nil {
				return fmt.Errorf("normalize target for %s: %w", entry.Agent, normalizeErr)
			}
			entry.Target = normalized
		}
		probeErr := selected.Probe(ctx, entry.Target)
		if probeErr == nil {
			return fmt.Errorf("refusing to retire %s wake: adapter target %s still exists", entry.Agent, entry.Target)
		}
		if !errors.Is(probeErr, adapter.ErrTargetNotFound) {
			return fmt.Errorf("refusing to retire %s wake because target absence is not proven: %w", entry.Agent, probeErr)
		}
	}

	cli := amq.NewCLI(*amqPath)
	result := retireSessionResult{Root: root, Adapter: *adapterName}
	forgetRetired := func() error {
		ids := make([]string, 0, len(result.Entries))
		for _, entry := range result.Entries {
			ids = append(ids, entry.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		removed, forgetErr := store.ForgetMany(ids)
		if forgetErr != nil {
			return forgetErr
		}
		if removed != len(ids) {
			return fmt.Errorf("removed %d, want %d", removed, len(ids))
		}
		return nil
	}
	for _, entry := range entries {
		retired, retireErr := cli.RetireWake(ctx, amq.RetireWakeRequest{
			Root:      root,
			Me:        entry.Agent,
			InjectVia: *self,
			Adapter:   entry.Adapter,
			Target:    entry.Target,
		})
		if retireErr != nil {
			if forgetErr := forgetRetired(); forgetErr != nil {
				return fmt.Errorf("retire %s wake: %v; also failed to forget already-retired entries: %w", entry.Agent, retireErr, forgetErr)
			}
			return fmt.Errorf("retire %s wake: %w", entry.Agent, retireErr)
		}
		if retired.Status != "retired" {
			if forgetErr := forgetRetired(); forgetErr != nil {
				return fmt.Errorf("retire %s wake returned unexpected status %q; also failed to forget already-retired entries: %w", entry.Agent, retired.Status, forgetErr)
			}
			return fmt.Errorf("retire %s wake returned unexpected status %q", entry.Agent, retired.Status)
		}
		result.Entries = append(result.Entries, retiredSessionEntry{
			ID: entry.ID, Agent: entry.Agent, Target: entry.Target, Status: retired.Status, PID: retired.PID,
		})
	}
	if err := forgetRetired(); err != nil {
		return fmt.Errorf("forget retired session registry entries: %w", err)
	}
	return printJSON(a.Stdout, result)
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
	interval := fs.Duration("interval", 10*time.Second, "supervisor interval")
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
	_, _ = fmt.Fprintln(writer, "usage: amq-keepalive <attach|reattach|supervise|inject|doctor|retire-session|forget|install-launchd|install-hook|uninstall> [options]")
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
