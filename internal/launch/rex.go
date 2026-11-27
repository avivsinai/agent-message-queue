package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	rexProfileVersion      = 1
	rexProfileVersionRange = "api_revision >=24"
	rexMinAPIRevision      = 24
	rexInstancePrefix      = "rex-server:"
	rexWindowPrefix        = "rex:v1:window:"
	rexBlockPrefix         = "rex:v1:block:"
	rexTerminalCreator     = "com.superlogical.terminal"
	rexResumeTokenEnv      = "OSC7577_TOKEN"
)

// RexBackend manages one Rex tab per AMQ project/session generation.
type RexBackend struct {
	backendTiming
	binary   string
	run      func(context.Context, ...string) (string, error)
	hostname func() (string, error)
	cli      RexLocator
}

func NewRexBackend(binary string) *RexBackend {
	b := &RexBackend{binary: strings.TrimSpace(binary), hostname: os.Hostname}
	b.run = b.runCommand
	return b
}

func RexProfile() Profile {
	return Profile{Backend: LauncherRex, Platform: "darwin", VersionRange: rexProfileVersionRange,
		Version: rexProfileVersion, Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus, CapReclaim}}
}

type rexProbe struct {
	InstanceID  string  `json:"instance_id"`
	APIRevision *int    `json:"api_revision"`
	SessionID   *string `json:"session_id"`
	WindowID    *string `json:"window_id"`
	BlockID     *string `json:"block_id"`
}

func (b *RexBackend) probe(ctx context.Context) (rexProbe, error) {
	raw, err := b.run(ctx, "whoami", "--json")
	if err != nil {
		return rexProbe{}, err
	}
	var p rexProbe
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("parse rex whoami: %w", err)
	}
	if !validUUID(p.InstanceID) || p.APIRevision == nil {
		return p, fmt.Errorf("rex whoami requires instance_id UUID and integer api_revision")
	}
	p.InstanceID = strings.ToLower(p.InstanceID)
	for _, field := range []struct {
		kind string
		id   **string
	}{{"session", &p.SessionID}, {"window", &p.WindowID}, {"block", &p.BlockID}} {
		if *field.id == nil {
			continue
		}
		id, err := parseRexID(**field.id, field.kind)
		if err != nil {
			return p, err
		}
		*field.id = &id
	}
	return p, nil
}

func (b *RexBackend) Detect() DetectResult {
	ctx, cancel := b.commandCallContext()
	defer cancel()
	p, err := b.probe(ctx)
	if err != nil {
		return DetectResult{Profile: RexProfile()}
	}
	return b.detectProbe(p)
}

func (b *RexBackend) detectProbe(p rexProbe) DetectResult {
	result := DetectResult{Profile: RexProfile(), InstanceIdentity: rexInstancePrefix + p.InstanceID}
	// Instance identity is the server process generation: Rex restores pane IDs
	// after restart with new programs, so older bindings belong to another context.
	host, err := b.hostname()
	result.HostIdentity = host
	switch {
	case err != nil || strings.TrimSpace(host) == "":
		rexUnsupported(&result, "rex host identity is unavailable")
	case *p.APIRevision < rexMinAPIRevision:
		rexUnsupported(&result, fmt.Sprintf("rex api revision %d is unsupported (want >=24)", *p.APIRevision))
	case p.SessionID == nil || p.BlockID == nil:
		rexUnsupported(&result, "rex launcher must run inside a Rex pane (REX_BLOCK names no pane on this server)")
	default:
		result.Available = true
		result.Effective = append([]Capability(nil), result.Profile.Capabilities...)
	}
	return result
}

func rexUnsupported(result *DetectResult, reason string) {
	for _, cap := range result.Profile.Capabilities {
		result.Degradations = append(result.Degradations, Degradation{Capability: cap, Reason: reason})
	}
}

func (b *RexBackend) Create(req CreateRequest) (CreateResult, error) {
	if req.JoinBinding != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: placementError(PlacementUnsupportedReason)}
	}
	preview, err := resolveCreatePlacement(LauncherRex, req)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	if err := validateCreate(req); err != nil {
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
	ctx, cancel := b.commandCallContext()
	p, err := b.probe(ctx)
	cancel()
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: fmt.Errorf("rex api_revision >=24 inside a Rex pane is unavailable: %w", err)}
	}
	detect := b.detectProbe(p)
	if !detect.Available {
		return CreateResult{}, &DefinitePreCreateError{Err: errors.New(detect.Degradations[0].Reason)}
	}
	session := *p.SessionID
	nonce := req.Plan.Agents[0].LaunchNonce
	name, err := rexWindowName(req.ProjectRoot, req.Session, nonce)
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	inspectCtx, inspectCancel := b.inspectCallContext()
	before, err := b.listWindows(inspectCtx)
	inspectCancel()
	if err != nil {
		return CreateResult{}, &DefinitePreCreateError{Err: err}
	}
	for _, window := range before {
		if window.Label == name {
			return CreateResult{}, fmt.Errorf("rex window %q already exists; journal recovery must classify it", name)
		}
	}
	createCtx, createCancel := b.createCallContext()
	raw, err := b.run(createCtx, "window", "new", "-s", "session:"+session, name, "-c", req.ProjectRoot, "--focus=false", "--json")
	createCancel()
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure(session, "", false, fmt.Errorf("create rex window: %w", err))
	}
	windowID, blocks, err := parseRexCreate(raw, true)
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure(session, "", false, err)
	}
	renameCtx, renameCancel := b.inspectCallContext()
	_, err = b.run(renameCtx, "block", "rename", "-s", "session:"+session, "block:"+blocks[0], req.Plan.Agents[0].Handle)
	renameCancel()
	if err != nil {
		return CreateResult{}, b.reconcileCreateFailure(session, windowID, false, fmt.Errorf("label rex pane for %s: %w", req.Plan.Agents[0].Handle, err))
	}
	direction := paneSplitDirection(preview.Effective.Layout)
	if direction == "down" {
		direction = "below"
	}
	for i, agent := range req.Plan.Agents[1:] {
		if err := b.staggerPlacement(preview, i+1); err != nil {
			return CreateResult{}, b.reconcileCreateFailure(session, windowID, false, err)
		}
		splitCtx, splitCancel := b.createCallContext()
		raw, err := b.run(splitCtx, "split", "-s", "session:"+session, "-b", "block:"+blocks[len(blocks)-1], "--split="+direction, "--focus=false", "-c", req.ProjectRoot, "--label", agent.Handle, "--json")
		splitCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(session, windowID, false, fmt.Errorf("create rex split for %s: %w", agent.Handle, err))
		}
		_, added, err := parseRexCreate(raw, false)
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(session, windowID, false, fmt.Errorf("rex split for %s: %w", agent.Handle, err))
		}
		blocks = append(blocks, added...)
	}
	if err := b.waitHealthy(context.Background(), session, blocks); err != nil {
		return CreateResult{}, b.reconcileCreateFailure(session, windowID, false, err)
	}
	for i, agent := range req.Plan.Agents {
		agent.EnvOverlay = cloneEnv(agent.EnvOverlay)
		if agent.EnvOverlay == nil {
			agent.EnvOverlay = make(map[string]string)
		}
		// An empty Program Resume token prevents terminal-side resume outside AMQ,
		// without queue identity or wake; AMQ alone owns managed seat resume.
		agent.EnvOverlay[rexResumeTokenEnv] = ""
		line := agentCommand(req, agent)
		sendCtx, sendCancel := b.inspectCallContext()
		_, err := b.run(sendCtx, "send", "-s", "session:"+session, "-b", "block:"+blocks[i], "--", line)
		sendCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(session, windowID, true, fmt.Errorf("send rex command for %s: %w", agent.Handle, err))
		}
		enterCtx, enterCancel := b.inspectCallContext()
		_, err = b.run(enterCtx, "send-key", "-s", "session:"+session, "-b", "block:"+blocks[i], "enter")
		enterCancel()
		if err != nil {
			return CreateResult{}, b.reconcileCreateFailure(session, windowID, true, fmt.Errorf("submit rex command for %s: %w", agent.Handle, err))
		}
	}
	resources := rexBindingResources(windowID, nonce, req.Plan, blocks)
	if issues := agentInventoryIssues(resources, req.Plan); len(issues) != 0 {
		return CreateResult{}, fmt.Errorf("created rex inventory differs from plan: %s", strings.Join(issues, ","))
	}
	binding := BindingRecord{Version: BindingVersion, Backend: LauncherRex, HostIdentity: detect.HostIdentity,
		InstanceIdentity: detect.InstanceIdentity, Profile: detect.Profile.Identity(), LaunchNonce: nonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources}, Placement: preview}
	if err := binding.Validate(); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Outcome: OutcomeCreated, Profile: binding.Profile, Binding: binding}, nil
}

func (b *RexBackend) reconcileCreateFailure(session, knownWindowID string, inputSent bool, cause error) error {
	if inputSent {
		return cause
	}
	if knownWindowID == "" {
		return fmt.Errorf("%w (rex window ownership unknown, never guessed)", cause)
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendCreateTimeout)
	defer cancel()
	windows, err := b.listWindows(ctx)
	if err != nil {
		return fmt.Errorf("%w (orphan window reconciliation failed: %v)", cause, err)
	}
	for _, window := range windows {
		if window.ID != knownWindowID {
			continue
		}
		if _, err := b.run(ctx, "window", "close", "-s", "session:"+session, "window:"+knownWindowID); err != nil {
			return fmt.Errorf("%w (close orphan %s: %v)", cause, knownWindowID, err)
		}
		return fmt.Errorf("%w (closed orphan rex window %s)", cause, knownWindowID)
	}
	return fmt.Errorf("%w (acknowledged rex window %s is absent)", cause, knownWindowID)
}

func (b *RexBackend) waitHealthy(parent context.Context, session string, blocks []string) error {
	timeout, poll := b.healthBudget()
	deadline := time.Now().Add(timeout)
	for {
		ready := true
		var lastErr error
		for _, block := range blocks {
			ctx, cancel := context.WithTimeout(parent, backendCommandTimeout)
			raw, err := b.run(ctx, "block", "call", "-s", "session:"+session, "-b", "block:"+block, "process")
			cancel()
			var process struct {
				Foreground *struct {
					PID  int    `json:"pid"`
					Name string `json:"name"`
				} `json:"foreground"`
			}
			if err == nil {
				err = json.Unmarshal([]byte(raw), &process)
			}
			// The typed line must reach a shell, not login(1).
			if err != nil || process.Foreground == nil || process.Foreground.PID <= 0 || process.Foreground.Name == "login" || process.Foreground.Name == "" {
				ready = false
				lastErr = err
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("rex pane readiness timed out: %w", lastErr)
			}
			return fmt.Errorf("rex pane readiness timed out before sending commands")
		}
		if err := b.doSleep(parent, poll); err != nil {
			return err
		}
	}
}

func (b *RexBackend) Inspect(req InspectRequest) (InspectResult, error) {
	return inspectBinding(req.Binding, b.inspect)
}

func (b *RexBackend) inspect(ctx context.Context, binding BindingRecord) (InspectResult, error) {
	if err := binding.Validate(); err != nil {
		return InspectResult{}, err
	}
	if err := checkBindingContext(binding, LauncherRex, RexProfile(), b.hostname, func() string { return b.Detect().InstanceIdentity }, "rex server is unreachable"); err != nil {
		return InspectResult{}, err
	}
	windowID, nonce, err := parseRexWindowResource(binding)
	if err != nil {
		return InspectResult{}, err
	}
	if nonce != binding.LaunchNonce {
		return InspectResult{Status: InspectUnknown, ActionRequired: true, Evidence: "rex window nonce differs from binding"}, nil
	}
	want, err := rexBoundBlocks(binding)
	if err != nil {
		return InspectResult{}, err
	}
	if len(want) == 0 {
		return InspectResult{}, fmt.Errorf("rex binding has no pane identities")
	}
	windows, err := b.listWindows(ctx)
	if err != nil {
		return InspectResult{}, err
	}
	for _, window := range windows {
		if window.ID != windowID {
			continue
		}
		if sameStringSet(window.Terminals, want) {
			return InspectResult{Status: InspectPresent, Evidence: "rex window and pane inventory match"}, nil
		}
		return InspectResult{Status: InspectUnknown, ActionRequired: true, Evidence: "rex window exists but pane inventory differs from binding"}, nil
	}
	if rexLiveOwned(windows, want) > 0 {
		return InspectResult{Status: InspectUnknown, ActionRequired: true, Evidence: "rex window is absent while bound panes remain"}, nil
	}
	return InspectResult{Status: InspectAbsent, Evidence: "rex window is absent"}, nil
}

func (b *RexBackend) Close(req CloseRequest) (CloseResult, error) {
	owned, _ := rexBoundBlocks(req.Binding)
	return ownedTerminalClose{backend: "rex", container: "window", terminal: "pane",
		inspect: func(ctx context.Context) (InspectResult, error) { return b.inspect(ctx, req.Binding) }, owned: owned,
		containerID: func() (string, error) { id, _, err := parseRexWindowResource(req.Binding); return id, err },
		closeTerminal: func(ctx context.Context, id string) error {
			_, err := b.run(ctx, "block", "close", "-s", "block:"+id, "block:"+id)
			return err
		},
		targetMissing: func(err error) bool { return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") },
		stillLive: func(ctx context.Context, _ string, owned []string) (int, error) {
			windows, err := b.listWindows(ctx)
			if err != nil {
				return 0, err
			}
			return rexLiveOwned(windows, owned), nil
		},
	}.run()
}

func (b *RexBackend) Focus(req FocusRequest) (FocusResult, error) {
	ctx, cancel := b.commandCallContext()
	defer cancel()
	inspection, err := b.inspect(ctx, req.Binding)
	if err != nil {
		return FocusResult{}, err
	}
	if inspection.Status != InspectPresent {
		return FocusResult{Outcome: OutcomeActionRequired, Reason: inspection.Evidence}, nil
	}
	blocks, err := rexBoundBlocks(req.Binding)
	if err != nil {
		return FocusResult{}, err
	}
	id := "block:" + blocks[0]
	if _, err := b.run(ctx, "focus", "-s", id, id); err != nil {
		return FocusResult{}, err
	}
	return FocusResult{Outcome: OutcomeAttached}, nil
}

func (b *RexBackend) Reclaim(req ReclaimRequest) (ReclaimResult, error) {
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, backendCommandTimeout)
	defer cancel()
	if req.Root == nil {
		return ReclaimResult{}, fmt.Errorf("missing pinned session root")
	}
	rootIdentity, err := canonicalIdentity(req.Root.Base())
	if err != nil || rootIdentity != req.Journal.RootIdentity {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "journal session root does not match the pinned root"}, nil
	}
	if req.Journal.RootPhysical != "" {
		physical, err := fsq.StableTreeIdentityInfo(req.Root.FileInfo())
		if err != nil || !fsq.SamePersistedTreeIdentity(req.Journal.RootPhysical, physical) {
			return ReclaimResult{Status: ReclaimForeign, Evidence: "journal session root identity changed"}, nil
		}
	}
	name, err := rexWindowName(req.Journal.ProjectIdentity, req.Journal.Session, req.Journal.LaunchNonce)
	if err != nil {
		return ReclaimResult{}, err
	}
	windows, err := b.listWindows(ctx)
	if err != nil {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: err.Error()}, nil
	}
	var matches []rexListedWindow
	for _, window := range windows {
		if window.Label == name {
			matches = append(matches, window)
		}
	}
	if req.Journal.Phase == JournalIntent {
		if len(matches) == 0 {
			return ReclaimResult{Status: ReclaimAbsent, Evidence: "deterministic rex window is absent"}, nil
		}
		return ReclaimResult{Status: ReclaimForeign, Evidence: "journal intent must not adopt a same-name rex window"}, nil
	}
	if len(matches) == 0 {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: "deterministic rex window is absent"}, nil
	}
	if len(matches) != 1 {
		return ReclaimResult{Status: ReclaimUnknown, Evidence: fmt.Sprintf("ambiguous rex windows named %q", name)}, nil
	}
	resources := rexBindingResources(matches[0].ID, req.Journal.LaunchNonce, req.Journal.Plan, matches[0].Terminals)
	issues := agentInventoryIssues(resources, req.Journal.Plan)
	if len(matches[0].Terminals) > len(req.Journal.Plan.Agents) {
		issues = append(issues, "unexpected:unassigned panes")
	}
	if len(issues) != 0 {
		return ReclaimResult{Status: ReclaimIncomplete, Evidence: "rex window inventory differs from plan: " + strings.Join(issues, ","), Resources: resources}, nil
	}
	binding := BindingRecord{Version: BindingVersion, Backend: LauncherRex, HostIdentity: req.Journal.HostIdentity,
		InstanceIdentity: req.Journal.InstanceIdentity, Profile: req.Journal.Profile, LaunchNonce: req.Journal.LaunchNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: resources}, Placement: req.Journal.Placement}
	p, probeErr := b.probe(ctx)
	host, hostErr := b.hostname()
	if probeErr != nil || hostErr != nil || strings.TrimSpace(host) == "" || host != binding.HostIdentity ||
		rexInstancePrefix+p.InstanceID != binding.InstanceIdentity || p.APIRevision == nil || *p.APIRevision < rexMinAPIRevision || RexProfile().Identity() != binding.Profile {
		return ReclaimResult{Status: ReclaimForeign, Evidence: "rex runtime identity changed", Resources: resources}, nil
	}
	return ReclaimResult{Status: ReclaimAdoptable, Evidence: "rex window and nonce match the complete journal plan", Resources: resources, Binding: binding}, nil
}

// rexListedWindow is one tab. Terminals are its terminal panes in creation
// order; Blocks is every pane, so a bound pane that changed type still counts
// as live.
type rexListedWindow struct {
	ID        string
	Label     string
	Terminals []string
	Blocks    []string
}

func (b *RexBackend) listWindows(ctx context.Context) ([]rexListedWindow, error) {
	raw, err := b.run(ctx, "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("list rex sessions: %w", err)
	}
	var parsed struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse rex sessions: %w", err)
	}
	if parsed.Sessions == nil {
		return nil, fmt.Errorf("rex sessions list is missing")
	}
	var windows []rexListedWindow
	for _, session := range parsed.Sessions {
		id, err := parseRexID(session.ID, "session")
		if err != nil {
			return nil, err
		}
		raw, err := b.run(ctx, "window", "ls", "-s", "session:"+id, "--json")
		if err != nil {
			return nil, fmt.Errorf("list rex windows: %w", err)
		}
		listed, err := parseRexWindows(raw, id)
		if err != nil {
			return nil, err
		}
		windows = append(windows, listed...)
	}
	return windows, nil
}

func parseRexWindows(raw, sessionID string) ([]rexListedWindow, error) {
	var parsed struct {
		SessionID string `json:"session_id"`
		Windows   []struct {
			ID     string `json:"window_id"`
			Label  string `json:"label"`
			Layers []struct {
				Blocks []struct {
					ID      string `json:"block_id"`
					Creator string `json:"creator_name"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse rex windows: %w", err)
	}
	id, err := parseRexID(parsed.SessionID, "session")
	if err != nil {
		return nil, err
	}
	if id != sessionID {
		return nil, fmt.Errorf("rex window listing belongs to another session")
	}
	if parsed.Windows == nil {
		return nil, fmt.Errorf("rex windows list is missing")
	}
	windows := make([]rexListedWindow, 0, len(parsed.Windows))
	for _, window := range parsed.Windows {
		id, err := parseRexID(window.ID, "window")
		if err != nil {
			return nil, err
		}
		record := rexListedWindow{ID: id, Label: window.Label}
		if window.Layers == nil {
			return nil, fmt.Errorf("rex layers list is missing")
		}
		for _, layer := range window.Layers {
			if layer.Blocks == nil {
				return nil, fmt.Errorf("rex blocks list is missing")
			}
			for _, block := range layer.Blocks {
				id, err := parseRexID(block.ID, "block")
				if err != nil {
					return nil, err
				}
				record.Blocks = append(record.Blocks, id)
				if block.Creator == rexTerminalCreator {
					record.Terminals = append(record.Terminals, id)
				}
			}
		}
		windows = append(windows, record)
	}
	return windows, nil
}

func parseRexID(raw, kind string) (string, error) {
	id, ok := strings.CutPrefix(raw, kind+":")
	if !ok || !validUUID(id) {
		return "", fmt.Errorf("invalid rex %s identity %q", kind, raw)
	}
	return strings.ToLower(id), nil
}

func parseRexCreate(raw string, window bool) (string, []string, error) {
	var parsed struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", nil, fmt.Errorf("parse rex create: %w", err)
	}
	id := ""
	if window {
		var err error
		id, err = parseRexID(parsed.WindowID, "window")
		if err != nil {
			return "", nil, err
		}
	}
	if len(parsed.BlockIDs) != 1 {
		return "", nil, fmt.Errorf("rex create returned %d panes, want 1", len(parsed.BlockIDs))
	}
	block, err := parseRexID(parsed.BlockIDs[0], "block")
	if err != nil {
		return "", nil, err
	}
	return id, []string{block}, nil
}

func rexWindowName(projectRoot, session, nonce string) (string, error) {
	canonical, err := canonicalIdentity(projectRoot)
	if err != nil {
		return "", err
	}
	if !validUUID(nonce) {
		return "", fmt.Errorf("launch nonce must be a UUID")
	}
	return fmt.Sprintf("amq-%s-%s-%s", truncateTMuxPart(tmuxSlug(filepath.Base(canonical)), 20), truncateTMuxPart(tmuxSlug(session), 16), nonce), nil
}

func rexBindingResources(windowID, nonce string, plan Plan, blocks []string) []ResourceIdentity {
	resources := []ResourceIdentity{{OpaqueID: rexWindowPrefix + windowID + ":" + nonce}}
	for i, block := range blocks {
		resource := ResourceIdentity{OpaqueID: rexBlockPrefix + block}
		if i < len(plan.Agents) {
			resource.Agent = plan.Agents[i].Handle
		}
		resources = append(resources, resource)
	}
	return resources
}

func parseRexWindowResource(binding BindingRecord) (string, string, error) {
	for _, resource := range binding.Resources.Resources {
		rest, ok := strings.CutPrefix(resource.OpaqueID, rexWindowPrefix)
		if !ok || resource.Agent != "" {
			continue
		}
		id, nonce, found := strings.Cut(rest, ":")
		if !found || !validUUID(id) || !validUUID(nonce) {
			return "", "", fmt.Errorf("binding has no valid rex window identity")
		}
		return strings.ToLower(id), nonce, nil
	}
	return "", "", fmt.Errorf("binding has no valid rex window identity")
}

func rexBoundBlocks(binding BindingRecord) ([]string, error) {
	var blocks []string
	for _, resource := range binding.Resources.Resources {
		if resource.Agent == "" {
			continue
		}
		id, ok := strings.CutPrefix(resource.OpaqueID, rexBlockPrefix)
		if !ok || !validUUID(id) {
			return nil, fmt.Errorf("invalid rex pane identity %q", resource.OpaqueID)
		}
		blocks = append(blocks, strings.ToLower(id))
	}
	return blocks, nil
}

func rexLiveOwned(windows []rexListedWindow, owned []string) int {
	live := make(map[string]bool)
	for _, window := range windows {
		for _, block := range window.Blocks {
			live[block] = true
		}
	}
	count := 0
	for _, id := range owned {
		if live[id] {
			count++
		}
	}
	return count
}

func (b *RexBackend) runCommand(ctx context.Context, args ...string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("rex backend requires macOS")
	}
	if len(args) == 0 {
		return "", fmt.Errorf("rex command is required")
	}
	path, err := b.resolveExecutable()
	if err != nil {
		return "", err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, backendCommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, path, append([]string{"--autostart=false"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("rex %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (b *RexBackend) resolveExecutable() (string, error) {
	if b.binary != "" {
		return filepath.Clean(b.binary), nil
	}
	return b.cli.Resolve()
}

// RexLocator finds the CLI of the running Rex installation. Nil fields use the OS.
type RexLocator struct {
	Getenv       func(string) string
	LookPath     func(string) (string, error)
	UserHomeDir  func() (string, error)
	IsExecutable func(string) bool
}

func (l RexLocator) Resolve() (string, error) {
	getenv := l.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	lookPath := l.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	userHomeDir := l.UserHomeDir
	if userHomeDir == nil {
		userHomeDir = os.UserHomeDir
	}
	if dir := strings.TrimSpace(getenv("REX_BIN_DIR")); dir != "" {
		path := filepath.Join(dir, "rex")
		if l.Executable(path) {
			return filepath.Clean(path), nil
		}
	}
	if path, err := lookPath("rex"); err == nil {
		return filepath.Clean(path), nil
	}
	if home, err := userHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		path := filepath.Join(home, "Library", "Application Support", "com.superlogical.rex", "RexServer", "Versions", "Current", "Rex Server.app", "Contents", "MacOS", "rex")
		if l.Executable(path) {
			return filepath.Clean(path), nil
		}
	}
	return "", errors.New("rex CLI not found")
}

func (l RexLocator) Executable(path string) bool {
	if l.IsExecutable != nil {
		return l.IsExecutable(path)
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
