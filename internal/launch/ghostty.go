package launch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	ghosttyProfileVersion      = 1
	ghosttyProfileVersionRange = ">=1.3.0 <2.0"
	ghosttyCommandTimeout      = 5 * time.Second
	ghosttyCreateTimeout       = 30 * time.Second
	ghosttyHealthTimeout       = 15 * time.Second
	ghosttyHealthPoll          = 100 * time.Millisecond
	ghosttyBundleID            = "com.mitchellh.ghostty"
	ghosttyInstancePrefix      = "ghostty-app:"
	ghosttyWindowPrefix        = "ghostty:v1:window:"
	ghosttyTabPrefix           = "ghostty:v1:tab:"
	ghosttyTerminalPrefix      = "ghostty:v1:terminal:"
)

// GhosttyBackend manages one Ghostty window per AMQ project/session generation
// on macOS via AppleScript.
type GhosttyBackend struct {
	run           func(context.Context, ...string) (string, error)
	hostname      func() (string, error)
	getenv        func(string) string
	sleep         func(context.Context, time.Duration) error
	createTimeout time.Duration
	healthTimeout time.Duration
	healthPoll    time.Duration
	mu            sync.Mutex
	generations   map[string]string
}

func NewGhosttyBackend() *GhosttyBackend {
	b := &GhosttyBackend{
		hostname: os.Hostname, getenv: os.Getenv,
		healthTimeout: ghosttyHealthTimeout, healthPoll: ghosttyHealthPoll,
		generations: make(map[string]string),
	}
	b.run = b.runAppleScript
	return b
}

func GhosttyProfile() Profile {
	return Profile{
		Backend: LauncherGhostty, Platform: "darwin",
		VersionRange: ghosttyProfileVersionRange, Version: ghosttyProfileVersion,
		Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus, CapReclaim},
	}
}

func (b *GhosttyBackend) Detect() DetectResult {
	profile := GhosttyProfile()
	result := DetectResult{Profile: profile}
	ctx, cancel := context.WithTimeout(context.Background(), ghosttyCommandTimeout)
	defer cancel()
	raw, err := b.run(ctx, "version")
	if err != nil {
		return result
	}
	host, err := b.hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return result
	}
	result.HostIdentity = host
	result.InstanceIdentity = ghosttyInstancePrefix + ghosttyBundleID
	if !supportedGhosttyVersion(raw) {
		reason := fmt.Sprintf("ghostty version %q is outside %s", strings.TrimSpace(raw), ghosttyProfileVersionRange)
		for _, cap := range profile.Capabilities {
			result.Degradations = append(result.Degradations, Degradation{Capability: cap, Reason: reason})
		}
		return result
	}
	result.Available = true
	result.Effective = append([]Capability(nil), profile.Capabilities...)
	return result
}

func (b *GhosttyBackend) Create(req CreateRequest) (CreateResult, error) {
	if req.JoinBinding != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
	}
	preview, err := resolveCreatePlacement(LauncherGhostty, req)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	if err := b.validateCreate(req); err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	amqPath := req.AMQPath
	if strings.TrimSpace(amqPath) == "" {
		amqPath = "amq"
	}
	resolvedAMQ, err := exec.LookPath(amqPath)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("resolve amq executable: %w", err)}
	}
	if resolvedAMQ, err = filepath.EvalSymlinks(resolvedAMQ); err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("resolve amq executable identity: %w", err)}
	}
	req.AMQPath = resolvedAMQ
	detect := b.Detect()
	if !detect.Available {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("ghostty %s is unavailable", ghosttyProfileVersionRange)}
	}
	nonce := req.Plan.Agents[0].LaunchNonce
	inspectCtx, inspectCancel := b.inspectCallContext()
	if existing, err := b.generationWindow(inspectCtx, nonce); err != nil {
		inspectCancel()
		return CreateResult{}, err
	} else if existing != "" {
		inspectCancel()
		return CreateResult{}, fmt.Errorf("ghostty window for launch nonce %s already exists; journal recovery must classify it", nonce)
	}
	before, err := b.listWindowIDs(inspectCtx)
	inspectCancel()
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("snapshot ghostty windows before create: %w", err)}
	}
	createCtx, createCancel := b.createCallContext()
	created, err := b.run(createCtx, "new-window", req.ProjectRoot)
	createCancel()
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure(before, "", false, fmt.Errorf("create ghostty window: %w", err))
	}
	windowID, tabID, firstTerminal, err := parseGhosttyCreatedWindow(created)
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure(before, "", false, err)
	}
	terminalIDs := []string{firstTerminal}
	for i, agent := range req.Plan.Agents[1:] {
		if err := b.staggerPlacement(preview, i+1); err != nil {
			return CreateResult{}, err
		}
		splitSnapCtx, splitSnapCancel := b.inspectCallContext()
		splitBefore, snapErr := b.listWindowIDs(splitSnapCtx)
		splitSnapCancel()
		if snapErr != nil {
			return CreateResult{}, b.reconcileCreateFailure(before, windowID, false, fmt.Errorf("snapshot ghostty windows before split: %w", snapErr))
		}
		splitCtx, splitCancel := b.createCallContext()
		splitRaw, splitErr := b.run(splitCtx, "split", terminalIDs[len(terminalIDs)-1], req.ProjectRoot, ghosttySplitDirection(preview.Effective.Layout))
		splitCancel()
		if splitErr != nil {
			return CreateResult{}, b.reconcileCreateFailure(splitBefore, windowID, false, fmt.Errorf("create ghostty split for %s: %w", agent.Handle, splitErr))
		}
		terminalID, splitErr := parseGhosttyTerminalID(splitRaw)
		if splitErr != nil {
			return CreateResult{}, b.reconcileCreateFailure(splitBefore, windowID, false, fmt.Errorf("ghostty split for %s: %w", agent.Handle, splitErr))
		}
		terminalIDs = append(terminalIDs, terminalID)
	}
	if err := b.waitHealthy(context.Background(), terminalIDs); err != nil {
		return CreateResult{}, b.reconcileCreateFailure(before, windowID, false, err)
	}
	for i, agent := range req.Plan.Agents {
		line := b.agentCommand(req, agent)
		inputCtx, inputCancel := b.inspectCallContext()
		_, err := b.run(inputCtx, "input-text", terminalIDs[i], line)
		inputCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(before, windowID, true, fmt.Errorf("send ghostty command for %s: %w", agent.Handle, err))
		}
		enterCtx, enterCancel := b.inspectCallContext()
		_, err = b.run(enterCtx, "send-key-enter", terminalIDs[i])
		enterCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(before, windowID, true, fmt.Errorf("submit ghostty command for %s: %w", agent.Handle, err))
		}
	}
	resources := ghosttyBindingResources(windowID, tabID, nonce, req.Plan, terminalIDs)
	if issues := ghosttyInventoryIssues(resources, req.Plan); len(issues) != 0 {
		return CreateResult{}, fmt.Errorf("created ghostty inventory differs from plan: %s", strings.Join(issues, ","))
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherGhostty, HostIdentity: detect.HostIdentity,
		InstanceIdentity: detect.InstanceIdentity, Profile: detect.Profile.Identity(),
		LaunchNonce: nonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
		Placement:   preview,
	}
	if err := binding.Validate(); err != nil {
		return CreateResult{}, err
	}
	b.rememberGeneration(nonce, windowID)
	return CreateResult{Outcome: OutcomeCreated, Profile: binding.Profile, Binding: binding}, nil
}

func (b *GhosttyBackend) createOpTimeout() time.Duration {
	if b.createTimeout > 0 {
		return b.createTimeout
	}
	return ghosttyCreateTimeout
}

func (b *GhosttyBackend) createCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), b.createOpTimeout())
}

func (b *GhosttyBackend) staggerPlacement(preview PlacementPreview, index int) error {
	delay := placementStaggerDuration(&preview.Effective)
	if delay <= 0 || index == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), delay+time.Second)
	defer cancel()
	return b.doSleep(ctx, delay)
}

func (b *GhosttyBackend) inspectCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), ghosttyCommandTimeout)
}

func (b *GhosttyBackend) reconcileCreateFailure(before []string, knownWindowID string, inputSent bool, cause error) error {
	if inputSent {
		return cause
	}
	ctx, cancel := b.inspectCallContext()
	defer cancel()
	listed, err := b.listWindowIDs(ctx)
	if err != nil {
		return fmt.Errorf("%w (orphan window reconciliation failed: %v)", cause, err)
	}
	newIDs := ghosttyIDsNotIn(listed, before)
	if knownWindowID != "" {
		extras := make([]string, 0, len(newIDs))
		for _, id := range newIDs {
			if id != knownWindowID {
				extras = append(extras, id)
			}
		}
		if containsString(listed, knownWindowID) {
			if _, closeErr := b.run(ctx, "close-window", knownWindowID); closeErr != nil {
				return fmt.Errorf("%w (close orphan %s: %v)", cause, knownWindowID, closeErr)
			}
		}
		if len(extras) > 0 {
			return fmt.Errorf("%w (closed orphan %s; ambiguous extra windows %s, unknown, never guessed)", cause, knownWindowID, strings.Join(extras, ","))
		}
		return fmt.Errorf("%w (closed orphan ghostty window %s)", cause, knownWindowID)
	}
	switch len(newIDs) {
	case 0:
		return fmt.Errorf("%w (no new ghostty window appeared)", cause)
	case 1:
		if _, closeErr := b.run(ctx, "close-window", newIDs[0]); closeErr != nil {
			return fmt.Errorf("%w (close orphan %s: %v)", cause, newIDs[0], closeErr)
		}
		return fmt.Errorf("%w (closed orphan ghostty window %s)", cause, newIDs[0])
	default:
		return fmt.Errorf("%w (ambiguous new ghostty windows %s; unknown, never guessed)", cause, strings.Join(newIDs, ","))
	}
}

func ghosttyIDsNotIn(listed, before []string) []string {
	out := make([]string, 0)
	for _, id := range listed {
		if !containsString(before, id) {
			out = append(out, id)
		}
	}
	return out
}

func (b *GhosttyBackend) Inspect(req InspectRequest) (InspectResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghosttyCommandTimeout)
	defer cancel()
	state, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return InspectResult{Status: InspectUnknown, Evidence: err.Error(), ActionRequired: true}, nil
	}
	return state, nil
}

func (b *GhosttyBackend) Close(req CloseRequest) (CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghosttyCommandTimeout)
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if inspection.Status == InspectAbsent {
		return CloseResult{Outcome: OutcomeClosed, Reason: "ghostty window already absent"}, nil
	}
	terminals := ghosttyBoundTerminals(req.Binding)
	if len(terminals) == 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: "ghostty binding has no terminal identities"}, nil
	}
	windowID, _, err := parseGhosttyWindowResource(req.Binding)
	if err != nil {
		windowID = ""
	}
	for _, terminalID := range terminals {
		if _, closeErr := b.run(ctx, "close-terminal", terminalID); closeErr != nil && !ghosttyTargetMissing(closeErr) {
			return CloseResult{}, fmt.Errorf("close ghostty terminal %s: %w", terminalID, closeErr)
		}
	}
	live, liveErr := b.countLiveTerminals(ctx, terminals)
	if liveErr != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: liveErr.Error()}, nil
	}
	if live > 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: "owned ghostty terminal still alive after close"}, nil
	}
	if windowID != "" {
		b.forgetGeneration(req.Binding.LaunchNonce, windowID)
	}
	return CloseResult{Outcome: OutcomeClosed, Reason: "ghostty terminals closed"}, nil
}

func (b *GhosttyBackend) Focus(req FocusRequest) (FocusResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghosttyCommandTimeout)
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil || inspection.Status != InspectPresent {
		if err != nil {
			return FocusResult{}, err
		}
		return FocusResult{Outcome: OutcomeActionRequired, Reason: inspection.Evidence}, nil
	}
	terminalID := firstGhosttyTerminalID(req.Binding)
	if terminalID == "" {
		return FocusResult{Outcome: OutcomeActionRequired, Reason: "ghostty binding has no terminal identity"}, nil
	}
	if _, err := b.run(ctx, "focus-terminal", terminalID); err != nil {
		return FocusResult{}, err
	}
	return FocusResult{Outcome: OutcomeAttached}, nil
}

func (b *GhosttyBackend) Reclaim(req ReclaimRequest) (ReclaimResult, error) {
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, ghosttyCommandTimeout)
	defer cancel()
	if req.Root == nil {
		return ReclaimResult{}, fmt.Errorf("missing pinned session root")
	}
	rootIdentity, err := canonicalIdentity(req.Root.Base())
	if err != nil || rootIdentity != req.Journal.RootIdentity {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "journal session root does not match the pinned root"}, nil
	}
	if req.Journal.RootPhysical != "" {
		physical, physicalErr := fsq.StableTreeIdentityInfo(req.Root.FileInfo())
		if physicalErr != nil || physical != req.Journal.RootPhysical {
			return ReclaimResult{Status: ReclaimForeign, Evidence: "journal session root identity changed"}, nil
		}
	}
	want, windowID, err := ghosttyJournalTerminals(req.Journal)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if len(want) == 0 {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: "journal has no ghostty terminal identities"}, nil
	}
	listed, err := b.listWindowIDs(ctx)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if windowID != "" && !containsString(listed, windowID) {
		live, liveErr := b.countLiveTerminals(ctx, want)
		if liveErr != nil {
			return ReclaimResult{Status: ReclaimUnknown, Evidence: liveErr.Error()}, nil
		}
		if live == 0 {
			return ReclaimResult{Status: ReclaimAbsent, Evidence: "journaled ghostty terminals are absent"}, nil
		}
		return ReclaimResult{Status: ReclaimUnknown, Evidence: "ghostty window id is absent while terminals remain"}, nil
	}
	if windowID == "" {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: "journal has no ghostty window identity"}, nil
	}
	live, err := b.windowTerminals(ctx, windowID)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	if !sameStringSet(live, want) {
		if len(live) == 0 && !containsString(listed, windowID) {
			return ReclaimResult{Status: ReclaimAbsent, Evidence: "journaled ghostty window is absent"}, nil
		}
		return ReclaimResult{
			Status: ReclaimIncomplete, Evidence: "ghostty terminal inventory differs from journal",
			Resources: ghosttyBindingResources(windowID, "", req.Journal.LaunchNonce, req.Journal.Plan, live),
		}, nil
	}
	resources := ghosttyBindingResources(windowID, ghosttyJournalTab(req.Journal), req.Journal.LaunchNonce, req.Journal.Plan, live)
	issues := ghosttyInventoryIssues(resources, req.Journal.Plan)
	if len(issues) != 0 {
		return ReclaimResult{
			Status: ReclaimIncomplete, Evidence: "ghostty window inventory differs from plan: " + strings.Join(issues, ","),
			Resources: resources,
		}, nil
	}
	detect := b.Detect()
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherGhostty, HostIdentity: req.Journal.HostIdentity,
		InstanceIdentity: req.Journal.InstanceIdentity, Profile: req.Journal.Profile,
		LaunchNonce: req.Journal.LaunchNonce,
		Resources:   ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources},
	}
	if !detect.Available || detect.HostIdentity != binding.HostIdentity ||
		detect.InstanceIdentity != binding.InstanceIdentity || GhosttyProfile().Identity() != binding.Profile {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "ghostty runtime identity changed", Resources: resources}, nil
	}
	return ReclaimResult{Status: ReclaimAdoptable, Evidence: "ghostty window and terminal UUIDs match the complete journal plan", Resources: resources, Binding: binding}, nil
}

func (b *GhosttyBackend) validateCreate(req CreateRequest) error {
	if strings.TrimSpace(req.ProjectRoot) == "" || strings.TrimSpace(req.Session) == "" || req.Root == nil {
		return fmt.Errorf("project root, session, and pinned root are required")
	}
	if err := req.Plan.Validate(); err != nil {
		return err
	}
	if _, err := canonicalIdentity(req.ProjectRoot); err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	handles := make([]string, len(req.Plan.Agents))
	nonce := req.Plan.Agents[0].LaunchNonce
	if !validUUID(nonce) {
		return fmt.Errorf("launch nonce must be a UUID")
	}
	for i, agent := range req.Plan.Agents {
		handles[i] = agent.Handle
		if agent.LaunchNonce != nonce {
			return fmt.Errorf("agent %q has a different launch nonce", agent.Handle)
		}
	}
	if err := fsq.ValidateExistingMailboxLayout(req.Root, handles...); err != nil {
		return err
	}
	return nil
}

func (b *GhosttyBackend) inspect(ctx context.Context, binding BindingRecord) (InspectResult, error) {
	if err := binding.Validate(); err != nil {
		return InspectResult{}, err
	}
	if err := b.validateBindingContext(binding); err != nil {
		return InspectResult{}, err
	}
	windowID, nonce, err := parseGhosttyWindowResource(binding)
	if err != nil {
		return InspectResult{}, err
	}
	if nonce != binding.LaunchNonce {
		return InspectResult{Status: InspectUnknown, Evidence: "ghostty window nonce differs from binding", ActionRequired: true}, nil
	}
	want := ghosttyBoundTerminals(binding)
	if len(want) == 0 {
		return InspectResult{}, fmt.Errorf("ghostty binding has no terminal identities")
	}
	listed, err := b.listWindowIDs(ctx)
	if err != nil {
		return InspectResult{}, err
	}
	if !containsString(listed, windowID) {
		return InspectResult{Status: InspectAbsent, Evidence: "ghostty window is absent"}, nil
	}
	live, err := b.windowTerminals(ctx, windowID)
	if err != nil {
		return InspectResult{}, err
	}
	if !sameStringSet(live, want) {
		return InspectResult{Status: InspectUnknown, Evidence: "ghostty window exists but terminal inventory differs from binding", ActionRequired: true}, nil
	}
	return InspectResult{Status: InspectPresent, Evidence: "ghostty window and terminal inventory match"}, nil
}

func (b *GhosttyBackend) validateBindingContext(binding BindingRecord) error {
	host, err := b.hostname()
	if err != nil {
		return fmt.Errorf("resolve ghostty host identity: %w", err)
	}
	if binding.Backend != LauncherGhostty || binding.Profile != GhosttyProfile().Identity() ||
		binding.HostIdentity != host {
		return fmt.Errorf("ghostty binding belongs to a different backend context")
	}
	detect := b.Detect()
	if detect.InstanceIdentity == "" {
		return fmt.Errorf("ghostty AppleScript is unreachable")
	}
	if binding.InstanceIdentity != detect.InstanceIdentity {
		return fmt.Errorf("ghostty binding belongs to a different backend context")
	}
	return nil
}

func (b *GhosttyBackend) waitHealthy(parent context.Context, terminalIDs []string) error {
	timeout := b.healthTimeout
	if timeout <= 0 {
		timeout = ghosttyHealthTimeout
	}
	poll := b.healthPoll
	if poll <= 0 {
		poll = ghosttyHealthPoll
	}
	deadline := time.Now().Add(timeout)
	for {
		ready := true
		var lastErr error
		for _, id := range terminalIDs {
			ctx, cancel := context.WithTimeout(parent, ghosttyCommandTimeout)
			raw, err := b.run(ctx, "terminal-count", id)
			cancel()
			if err != nil {
				lastErr = err
				ready = false
				break
			}
			count, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || count != 1 {
				ready = false
				if err != nil {
					lastErr = err
				}
				break
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("ghostty surface readiness timed out: %w", lastErr)
			}
			return fmt.Errorf("ghostty surface readiness timed out before sending commands")
		}
		if err := b.doSleep(parent, poll); err != nil {
			return err
		}
	}
}

func (b *GhosttyBackend) listWindowIDs(ctx context.Context) ([]string, error) {
	raw, err := b.run(ctx, "list-windows")
	if err != nil {
		return nil, fmt.Errorf("list ghostty windows: %w", err)
	}
	return splitGhosttyIDs(raw), nil
}

func (b *GhosttyBackend) windowTerminals(ctx context.Context, windowID string) ([]string, error) {
	raw, err := b.run(ctx, "window-terminals", windowID)
	if err != nil {
		return nil, fmt.Errorf("list ghostty window terminals: %w", err)
	}
	ids := splitGhosttyIDs(raw)
	for i, id := range ids {
		normalized, err := normalizeGhosttyUUID(id)
		if err != nil {
			return nil, fmt.Errorf("ghostty window terminal: %w", err)
		}
		ids[i] = normalized
	}
	return ids, nil
}

func (b *GhosttyBackend) countLiveTerminals(ctx context.Context, ids []string) (int, error) {
	live := 0
	for _, id := range ids {
		raw, err := b.run(ctx, "terminal-count", id)
		if err != nil {
			return 0, err
		}
		count, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return 0, err
		}
		if count > 0 {
			live++
		}
	}
	return live, nil
}

func (b *GhosttyBackend) agentCommand(req CreateRequest, agent AgentPlan) string {
	amq := req.AMQPath
	if strings.TrimSpace(amq) == "" {
		amq = "amq"
	}
	env := cloneEnv(agent.EnvOverlay)
	if env == nil {
		env = make(map[string]string, 1)
	}
	env[InternalLaunchNonceEnv] = agent.LaunchNonce
	return commandLine(agent.Cwd, env, coopExecArgv(amq, req.Root.Base(), agent.Handle, agent.Argv, agent.Execution))
}

func (b *GhosttyBackend) runAppleScript(ctx context.Context, args ...string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("ghostty backend requires macOS")
	}
	if len(args) == 0 {
		return "", fmt.Errorf("ghostty op is required")
	}
	script, err := ghosttyScript(args[0])
	if err != nil {
		return "", err
	}
	cmdArgs := append([]string{"-e", script}, args[1:]...)
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ghosttyCommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "osascript", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("osascript %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (b *GhosttyBackend) doSleep(ctx context.Context, delay time.Duration) error {
	if b.sleep != nil {
		return b.sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func ghosttyScript(op string) (string, error) {
	switch op {
	case "version":
		return `tell application "Ghostty" to get version`, nil
	case "new-window":
		return `
on run argv
	set cwd to item 1 of argv
	tell application "Ghostty"
		set cfg to new surface configuration
		set initial working directory of cfg to cwd
		set win to new window with configuration cfg
		set term to terminal 1 of selected tab of win
		return (id of win) & "|" & (id of selected tab of win) & "|" & (id of term)
	end tell
end run
`, nil
	case "split":
		return `
on run argv
	set targetID to item 1 of argv
	set cwd to item 2 of argv
	tell application "Ghostty"
		set cfg to new surface configuration
		set initial working directory of cfg to cwd
		set matches to terminals whose id is targetID
		if (count of matches) is not 1 then error "ghostty terminal not unique"
		set dir to "right"
		if (count of argv) is greater than 2 then set dir to item 3 of argv
		if dir is "down" then
			set newTerm to split (item 1 of matches) direction down with configuration cfg
		else
			set newTerm to split (item 1 of matches) direction right with configuration cfg
		end if
		return id of newTerm
	end tell
end run
`, nil
	case "list-windows":
		return `
tell application "Ghostty"
	set ids to {}
	repeat with w in windows
		set end of ids to id of w
	end repeat
	set AppleScript's text item delimiters to ","
	return ids as text
end tell
`, nil
	case "window-terminals":
		return `
on run argv
	set winID to item 1 of argv
	tell application "Ghostty"
		set wins to windows whose id is winID
		if (count of wins) is 0 then return ""
		set ids to {}
		repeat with t in terminals of (item 1 of wins)
			set end of ids to id of t
		end repeat
		set AppleScript's text item delimiters to ","
		return ids as text
	end tell
end run
`, nil
	case "terminal-count":
		return `
on run argv
	tell application "Ghostty"
		return count of (terminals whose id is (item 1 of argv))
	end tell
end run
`, nil
	case "input-text":
		return `
on run argv
	set targetID to item 1 of argv
	set payload to item 2 of argv
	tell application "Ghostty"
		set matches to terminals whose id is targetID
		if (count of matches) is not 1 then error "ghostty terminal not unique"
		input text payload to (item 1 of matches)
	end tell
end run
`, nil
	case "send-key-enter":
		return `
on run argv
	tell application "Ghostty"
		set matches to terminals whose id is (item 1 of argv)
		if (count of matches) is not 1 then error "ghostty terminal not unique"
		send key "enter" to (item 1 of matches)
	end tell
end run
`, nil
	case "focus-terminal":
		return `
on run argv
	tell application "Ghostty"
		set matches to terminals whose id is (item 1 of argv)
		if (count of matches) is not 1 then error "ghostty terminal not unique"
		focus (item 1 of matches)
	end tell
end run
`, nil
	case "close-window":
		return `
on run argv
	set winID to item 1 of argv
	tell application "Ghostty"
		set wins to windows whose id is winID
		if (count of wins) is 0 then error "ghostty window is absent"
		close window (item 1 of wins)
	end tell
end run
`, nil
	case "close-terminal":
		return `
on run argv
	set termID to item 1 of argv
	tell application "Ghostty"
		set matches to terminals whose id is termID
		if (count of matches) is 0 then return
		close (item 1 of matches)
	end tell
end run
`, nil
	default:
		return "", fmt.Errorf("unknown ghostty op %q", op)
	}
}

func parseGhosttyCreatedWindow(raw string) (string, string, string, error) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("ghostty new-window returned invalid identities %q", strings.TrimSpace(raw))
	}
	windowID, err := parseGhosttyOpaqueID(parts[0])
	if err != nil {
		return "", "", "", fmt.Errorf("ghostty window id: %w", err)
	}
	tabID, err := parseGhosttyOpaqueID(parts[1])
	if err != nil {
		return "", "", "", fmt.Errorf("ghostty tab id: %w", err)
	}
	terminalID, err := parseGhosttyTerminalID(parts[2])
	if err != nil {
		return "", "", "", err
	}
	return windowID, tabID, terminalID, nil
}

func parseGhosttyTerminalID(raw string) (string, error) {
	return normalizeGhosttyUUID(strings.TrimSpace(raw))
}

func parseGhosttyOpaqueID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" || strings.Contains(id, "/") || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid ghostty identity %q", raw)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("invalid ghostty identity %q", raw)
	}
	return id, nil
}

func normalizeGhosttyUUID(raw string) (string, error) {
	id := strings.ToUpper(strings.TrimSpace(raw))
	if !validUUID(id) {
		return "", fmt.Errorf("not a UUID: %q", raw)
	}
	return id, nil
}

func ghosttyBindingResources(windowID, tabID, nonce string, plan Plan, terminalIDs []string) []ResourceIdentity {
	resources := []ResourceIdentity{{OpaqueID: ghosttyWindowPrefix + windowID + ":" + nonce}}
	if tabID != "" {
		resources = append(resources, ResourceIdentity{OpaqueID: ghosttyTabPrefix + tabID})
	}
	for i, agent := range plan.Agents {
		if i < len(terminalIDs) {
			resources = append(resources, ResourceIdentity{OpaqueID: ghosttyTerminalPrefix + terminalIDs[i], Agent: agent.Handle})
		}
	}
	return resources
}

func parseGhosttyWindowResource(binding BindingRecord) (string, string, error) {
	for _, resource := range binding.Resources.Resources {
		rest, ok := strings.CutPrefix(resource.OpaqueID, ghosttyWindowPrefix)
		if !ok || resource.Agent != "" {
			continue
		}
		windowID, nonce, found := strings.Cut(rest, ":")
		if !found {
			return "", "", fmt.Errorf("binding has no valid ghostty window identity")
		}
		id, err := parseGhosttyOpaqueID(windowID)
		if err != nil || !validUUID(nonce) {
			return "", "", fmt.Errorf("binding has no valid ghostty window identity")
		}
		return id, nonce, nil
	}
	return "", "", fmt.Errorf("binding has no valid ghostty window identity")
}

func ghosttyBoundTerminals(binding BindingRecord) []string {
	var ids []string
	for _, resource := range binding.Resources.Resources {
		if resource.Agent == "" {
			continue
		}
		id, err := parseGhosttyTerminalResource(resource.OpaqueID)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func parseGhosttyTerminalResource(opaqueID string) (string, error) {
	id, ok := strings.CutPrefix(opaqueID, ghosttyTerminalPrefix)
	if !ok {
		return "", fmt.Errorf("invalid ghostty terminal identity %q", opaqueID)
	}
	return normalizeGhosttyUUID(id)
}

func firstGhosttyTerminalID(binding BindingRecord) string {
	ids := ghosttyBoundTerminals(binding)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func ghosttyJournalTerminals(journal LaunchJournal) ([]string, string, error) {
	if journal.Binding == nil {
		return nil, "", nil
	}
	windowID, _, err := parseGhosttyWindowResource(*journal.Binding)
	if err != nil {
		windowID = ""
	}
	return ghosttyBoundTerminals(*journal.Binding), windowID, nil
}

func ghosttyJournalTab(journal LaunchJournal) string {
	if journal.Binding == nil {
		return ""
	}
	for _, resource := range journal.Binding.Resources.Resources {
		id, ok := strings.CutPrefix(resource.OpaqueID, ghosttyTabPrefix)
		if ok && resource.Agent == "" {
			if parsed, err := parseGhosttyOpaqueID(id); err == nil {
				return parsed
			}
		}
	}
	return ""
}

func ghosttyInventoryIssues(resources []ResourceIdentity, plan Plan) []string {
	expected := make(map[string]bool, len(plan.Agents))
	for _, agent := range plan.Agents {
		expected[agent.Handle] = true
	}
	live := make(map[string]int, len(resources))
	issues := make([]string, 0)
	for _, resource := range resources {
		if resource.Agent != "" {
			live[resource.Agent]++
			if !expected[resource.Agent] {
				issues = append(issues, "unexpected:"+resource.Agent)
			}
		}
	}
	for _, agent := range plan.Agents {
		if live[agent.Handle] != 1 {
			issues = append(issues, "missing:"+agent.Handle)
		}
	}
	return issues
}

var ghosttyVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func supportedGhosttyVersion(raw string) bool {
	match := ghosttyVersionPattern.FindStringSubmatch(raw)
	if match == nil {
		return false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch := 0
	if match[3] != "" {
		patch, _ = strconv.Atoi(match[3])
	}
	if major != 1 || minor < 3 {
		return false
	}
	if minor == 3 && patch < 0 {
		return false
	}
	return true
}

func splitGhosttyIDs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		if id := strings.TrimSpace(part); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsString(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func ghosttyTargetMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "absent") || strings.Contains(msg, "not unique") || strings.Contains(msg, "can't get")
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, id := range left {
		counts[id]++
	}
	for _, id := range right {
		counts[id]--
		if counts[id] < 0 {
			return false
		}
	}
	return true
}

func (b *GhosttyBackend) rememberGeneration(nonce, windowID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.generations == nil {
		b.generations = make(map[string]string)
	}
	b.generations[nonce] = windowID
}

func (b *GhosttyBackend) forgetGeneration(nonce, windowID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.generations[nonce] == windowID {
		delete(b.generations, nonce)
	}
}

func (b *GhosttyBackend) generationWindow(ctx context.Context, nonce string) (string, error) {
	b.mu.Lock()
	windowID := b.generations[nonce]
	b.mu.Unlock()
	if windowID == "" {
		return "", nil
	}
	listed, err := b.listWindowIDs(ctx)
	if err != nil {
		return "", err
	}
	if containsString(listed, windowID) {
		return windowID, nil
	}
	b.forgetGeneration(nonce, windowID)
	return "", nil
}
