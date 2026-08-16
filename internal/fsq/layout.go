package fsq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// handleRe matches the historical safe single-component handle character set.
// ValidateHandle separately rejects leading '-' for live handles.
var handleRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidateHandle returns an error if the agent handle contains path traversal
// characters or does not match the allowed pattern.
func ValidateHandle(agent string) error {
	if err := validateLegacyInspectableHandle(agent); err != nil {
		return err
	}
	if strings.HasPrefix(agent, "-") {
		return fmt.Errorf("agent handle must not start with '-': %q", agent)
	}
	return nil
}

// ValidateLegacyHandleForInspection accepts the historical safe single-path
// component grammar. It exists only so read-only inventory/list operations can
// surface mailboxes created before leading '-' was rejected. It must not
// authorize creation, delivery, consumption, repair, wake, or presence writes.
func ValidateLegacyHandleForInspection(agent string) error {
	return validateLegacyInspectableHandle(agent)
}

func validateLegacyInspectableHandle(agent string) error {
	if agent == "" || strings.TrimSpace(agent) == "" {
		return fmt.Errorf("agent handle is empty")
	}
	if strings.Contains(agent, "..") || strings.Contains(agent, "/") || strings.Contains(agent, string(filepath.Separator)) {
		return fmt.Errorf("agent handle contains path traversal: %q", agent)
	}
	if !handleRe.MatchString(agent) {
		return fmt.Errorf("agent handle must match [a-z0-9_-]+: %q", agent)
	}
	return nil
}

// MailboxLeaf identifies one required per-agent mailbox leaf.
type MailboxLeaf string

const (
	MailboxInboxTmp   MailboxLeaf = "inbox/tmp"
	MailboxInboxNew   MailboxLeaf = "inbox/new"
	MailboxInboxCur   MailboxLeaf = "inbox/cur"
	MailboxOutboxSent MailboxLeaf = "outbox/sent"
	MailboxDLQTmp     MailboxLeaf = "dlq/tmp"
	MailboxDLQNew     MailboxLeaf = "dlq/new"
	MailboxDLQCur     MailboxLeaf = "dlq/cur"
	MailboxReceipts   MailboxLeaf = "receipts"
)

var requiredMailboxLeaves = [...]MailboxLeaf{
	MailboxInboxTmp,
	MailboxInboxNew,
	MailboxInboxCur,
	MailboxOutboxSent,
	MailboxDLQTmp,
	MailboxDLQNew,
	MailboxDLQCur,
	MailboxReceipts,
}

// RequiredMailboxLeaves returns the one ordered mailbox-layout contract.
func RequiredMailboxLeaves() []MailboxLeaf {
	leaves := make([]MailboxLeaf, len(requiredMailboxLeaves))
	copy(leaves, requiredMailboxLeaves[:])
	return leaves
}

// AgentMailboxPath returns one required leaf below an agent mailbox.
func AgentMailboxPath(root, agent string, leaf MailboxLeaf) string {
	return filepath.Join(root, "agents", agent, filepath.FromSlash(string(leaf)))
}

// MailboxRootRelativePath returns one required leaf relative to a queue root.
func MailboxRootRelativePath(agent string, leaf MailboxLeaf) string {
	return filepath.Join("agents", agent, filepath.FromSlash(string(leaf)))
}

// Path helpers for standard mailbox directories.

func AgentBase(root, agent string) string {
	return filepath.Join(root, "agents", agent)
}

func AgentInboxTmp(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxInboxTmp)
}

func AgentInboxNew(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxInboxNew)
}

func AgentInboxCur(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxInboxCur)
}

func AgentOutboxSent(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxOutboxSent)
}

func AgentDLQTmp(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxDLQTmp)
}

func AgentDLQNew(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxDLQNew)
}

func AgentDLQCur(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxDLQCur)
}

func AgentReceipts(root, agent string) string {
	return AgentMailboxPath(root, agent, MailboxReceipts)
}

func EnsureRootDirs(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "agents"),
		filepath.Join(root, "threads"),
		filepath.Join(root, "meta"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func EnsureAgentDirs(root, agent string) error {
	if err := ValidateHandle(agent); err != nil {
		return err
	}
	for _, leaf := range requiredMailboxLeaves {
		if err := os.MkdirAll(AgentMailboxPath(root, agent, leaf), 0o700); err != nil {
			return err
		}
	}
	return nil
}

type MailboxProvenance string

const (
	MailboxConfigured              MailboxProvenance = "configured"
	MailboxDiscovered              MailboxProvenance = "discovered"
	MailboxConfiguredAndDiscovered MailboxProvenance = "configured_and_discovered"
)

type MailboxPathState string

const (
	MailboxPathDirectory               MailboxPathState = "directory"
	MailboxPathMissing                 MailboxPathState = "missing"
	MailboxPathSymlink                 MailboxPathState = "symlink"
	MailboxPathNonDirectory            MailboxPathState = "non_directory"
	MailboxPathUnreadable              MailboxPathState = "unreadable"
	MailboxPathChangedDuringInspection MailboxPathState = "changed_during_inspection"
)

type MailboxPathInspection struct {
	Path  string           `json:"path"`
	State MailboxPathState `json:"state"`
	Mode  string           `json:"mode,omitempty"`
}

type MailboxInspection struct {
	Handle         string                  `json:"handle"`
	Provenance     MailboxProvenance       `json:"provenance"`
	Status         string                  `json:"status"`
	Issues         []string                `json:"issues"`
	Paths          []MailboxPathInspection `json:"paths,omitempty"`
	RepairEligible bool                    `json:"repair_eligible"`
	Remedy         string                  `json:"remedy,omitempty"`
	CreatedPaths   []string                `json:"created_paths,omitempty"`
}

type MailboxInventory struct {
	Mailboxes          []MailboxInspection
	ActiveConfigStatus string
	ActiveConfigIssue  string
	ConfiguredAgents   []string
	AgentsState        MailboxPathState
	AgentsIssue        string
	RepairAuthorized   bool
}

type MailboxRepairFailure struct {
	Code                    string `json:"code"`
	Stage                   string `json:"stage"`
	Path                    string `json:"path,omitempty"`
	Message                 string `json:"message"`
	DurabilityIndeterminate bool   `json:"durability_indeterminate,omitempty"`
}

type MailboxRepairResult struct {
	Status       string                `json:"status"`
	CreatedPaths []string              `json:"created_paths,omitempty"`
	Failure      *MailboxRepairFailure `json:"failure,omitempty"`
	Inventory    MailboxInventory      `json:"-"`
}

type mailboxActiveConfig struct {
	Agents []string `json:"agents"`
}

type mailboxConfigPin struct {
	root *DeliveryRoot
	file *os.File
	info os.FileInfo
	data []byte
	cfg  mailboxActiveConfig
}

// MailboxConfigAuthorization retains the exact config descriptor and content
// used to authorize mailbox repair. Callers must close it.
type MailboxConfigAuthorization struct {
	pin *mailboxConfigPin
}

type mailboxLayoutNode struct {
	state MailboxPathState
	mode  os.FileMode
	info  layoutNodeInfo
}

type mailboxLayoutPlan struct {
	inventory MailboxInventory
	nodes     map[string]mailboxLayoutNode
}

type mailboxRepairHooks struct {
	afterPreflight       func()
	afterFinalInspection func()
	fail                 func(stage, path string) error
}

var errLayoutIdentityChanged = errors.New("layout component changed during inspection")

// mailboxComponentPaths is parent-first and includes every unique intermediate
// plus the eight required leaves. "." is the per-agent directory itself.
func mailboxComponentPaths() []string {
	seen := map[string]bool{".": true}
	paths := []string{"."}
	for _, leaf := range requiredMailboxLeaves {
		parts := strings.Split(string(leaf), "/")
		for i := range parts {
			path := filepath.Join(parts[:i+1]...)
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func mailboxIssue(state MailboxPathState, path string) string {
	return string(state) + ":" + filepath.ToSlash(path)
}

func inspectDirectoryComponent(parent *layoutDirCapability, name, display string) (mailboxLayoutNode, *layoutDirCapability) {
	info, err := lstatLayoutNode(parent, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return mailboxLayoutNode{state: MailboxPathMissing}, nil
		}
		return mailboxLayoutNode{state: MailboxPathUnreadable}, nil
	}
	node := mailboxLayoutNode{mode: info.mode, info: info}
	switch info.kind {
	case layoutNodeSymlink:
		node.state = MailboxPathSymlink
		return node, nil
	case layoutNodeDirectory:
		child, err := openLayoutDirectory(parent, name, info)
		if err != nil {
			if errors.Is(err, errLayoutIdentityChanged) {
				node.state = MailboxPathChangedDuringInspection
			} else {
				node.state = MailboxPathUnreadable
			}
			return node, nil
		}
		node.state = MailboxPathDirectory
		return node, child
	default:
		node.state = MailboxPathNonDirectory
		return node, nil
	}
}

func readActiveMailboxConfig(root *layoutDirCapability) (mailboxActiveConfig, string, string) {
	metaNode, meta := inspectDirectoryComponent(root, "meta", "meta")
	if meta != nil {
		defer meta.close()
	}
	if metaNode.state != MailboxPathDirectory {
		status := string(metaNode.state)
		return mailboxActiveConfig{}, status, mailboxIssue(metaNode.state, "meta")
	}
	data, state, err := readLayoutRegularFile(meta, "config.json")
	if err != nil {
		if state == "" {
			state = MailboxPathUnreadable
		}
		return mailboxActiveConfig{}, string(state), mailboxIssue(state, "meta/config.json")
	}
	var cfg mailboxActiveConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return mailboxActiveConfig{}, "malformed", fmt.Sprintf("malformed:meta/config.json:%v", err)
	}
	for _, handle := range cfg.Agents {
		if err := ValidateHandle(handle); err != nil {
			return cfg, "invalid_handles", fmt.Sprintf("invalid_handle:%s:%v", handle, err)
		}
	}
	return cfg, "ok", ""
}

func pinActiveMailboxConfig(root *DeliveryRoot) (*mailboxConfigPin, string, string) {
	const configPath = "meta/config.json"
	file, info, err := root.OpenRegularNoFollow(configPath)
	if err != nil {
		state := MailboxPathUnreadable
		if errors.Is(err, fs.ErrNotExist) {
			state = MailboxPathMissing
		}
		issue := mailboxIssue(state, configPath)
		if state != MailboxPathMissing {
			issue += ":" + err.Error()
		}
		return nil, string(state), issue
	}
	data, err := io.ReadAll(file)
	if err != nil {
		_ = file.Close()
		return nil, string(MailboxPathUnreadable), mailboxIssue(MailboxPathUnreadable, configPath)
	}
	var cfg mailboxActiveConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		_ = file.Close()
		return nil, "malformed", fmt.Sprintf("malformed:%s:%v", configPath, err)
	}
	for _, handle := range cfg.Agents {
		if err := ValidateHandle(handle); err != nil {
			_ = file.Close()
			return nil, "invalid_handles", fmt.Sprintf("invalid_handle:%s:%v", handle, err)
		}
	}
	return &mailboxConfigPin{
		root: root,
		file: file,
		info: info,
		data: append([]byte(nil), data...),
		cfg:  cfg,
	}, "ok", ""
}

func (p *mailboxConfigPin) verify() error {
	if p == nil || p.file == nil || p.info == nil {
		return fmt.Errorf("mailbox config authorization is closed")
	}
	if err := p.root.VerifyBase(); err != nil {
		return err
	}
	info, err := p.file.Stat()
	if err != nil || !os.SameFile(p.info, info) {
		return fmt.Errorf("mailbox config descriptor identity changed")
	}
	if _, err := p.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind mailbox config descriptor: %w", err)
	}
	pinnedData, err := io.ReadAll(p.file)
	if err != nil {
		return fmt.Errorf("re-read mailbox config descriptor: %w", err)
	}
	if !bytes.Equal(pinnedData, p.data) {
		return fmt.Errorf("mailbox config content changed")
	}

	current, currentInfo, err := p.root.OpenRegularNoFollow("meta/config.json")
	if err != nil {
		return fmt.Errorf("re-open mailbox config: %w", err)
	}
	defer func() { _ = current.Close() }()
	if !os.SameFile(p.info, currentInfo) {
		return fmt.Errorf("mailbox config path identity changed")
	}
	currentData, err := io.ReadAll(current)
	if err != nil {
		return fmt.Errorf("re-read current mailbox config: %w", err)
	}
	if !bytes.Equal(currentData, p.data) {
		return fmt.Errorf("mailbox config path content changed")
	}
	return nil
}

// OpenMailboxConfigAuthorization pins the active config through configRoot.
func OpenMailboxConfigAuthorization(configRoot *DeliveryRoot) (*MailboxConfigAuthorization, MailboxInventory, error) {
	pin, status, issue := pinActiveMailboxConfig(configRoot)
	inventory := MailboxInventory{
		ActiveConfigStatus: status,
		ActiveConfigIssue:  issue,
		RepairAuthorized:   pin != nil,
	}
	if pin == nil {
		return nil, inventory, errors.New(issue)
	}
	inventory.ConfiguredAgents = append([]string(nil), pin.cfg.Agents...)
	return &MailboxConfigAuthorization{pin: pin}, inventory, nil
}

// Close releases the retained config descriptor.
func (a *MailboxConfigAuthorization) Close() error {
	if a == nil || a.pin == nil || a.pin.file == nil {
		return nil
	}
	return a.pin.file.Close()
}

// ConfiguredAgents returns the roster bound to this authorization.
func (a *MailboxConfigAuthorization) ConfiguredAgents() []string {
	if a == nil || a.pin == nil {
		return nil
	}
	return append([]string(nil), a.pin.cfg.Agents...)
}

// Verify confirms the retained descriptor, its content, and its current path
// still identify the exact config that authorized this operation.
func (a *MailboxConfigAuthorization) Verify() error {
	if a == nil || a.pin == nil {
		return fmt.Errorf("mailbox config authorization is missing")
	}
	return a.pin.verify()
}

func inspectMailboxLayout(root *DeliveryRoot, additionalHandles ...string) (mailboxLayoutPlan, error) {
	if err := root.VerifyBase(); err != nil {
		return mailboxLayoutPlan{}, err
	}
	rootCap, err := openLayoutRootCapability(root)
	if err != nil {
		return mailboxLayoutPlan{}, err
	}
	defer rootCap.close()

	cfg, configStatus, configIssue := readActiveMailboxConfig(rootCap)
	return inspectMailboxLayoutCapability(rootCap, cfg, configStatus, configIssue, additionalHandles...)
}

func inspectMailboxLayoutWithConfig(root *DeliveryRoot, cfg mailboxActiveConfig, additionalHandles ...string) (mailboxLayoutPlan, error) {
	if err := root.VerifyBase(); err != nil {
		return mailboxLayoutPlan{}, err
	}
	rootCap, err := openLayoutRootCapability(root)
	if err != nil {
		return mailboxLayoutPlan{}, err
	}
	defer rootCap.close()
	return inspectMailboxLayoutCapability(rootCap, cfg, "ok", "", additionalHandles...)
}

func inspectMailboxLayoutCapability(rootCap *layoutDirCapability, cfg mailboxActiveConfig, configStatus, configIssue string, additionalHandles ...string) (mailboxLayoutPlan, error) {
	inventory := MailboxInventory{
		ActiveConfigStatus: configStatus,
		ActiveConfigIssue:  configIssue,
		ConfiguredAgents:   append([]string(nil), cfg.Agents...),
		RepairAuthorized:   configStatus == "ok",
	}

	configured := make(map[string]bool, len(cfg.Agents))
	for _, handle := range cfg.Agents {
		configured[handle] = true
	}

	agentsNode, agentsCap := inspectDirectoryComponent(rootCap, "agents", "agents")
	if agentsCap != nil {
		defer agentsCap.close()
	}
	inventory.AgentsState = agentsNode.state
	if agentsNode.state != MailboxPathDirectory && agentsNode.state != MailboxPathMissing {
		inventory.AgentsIssue = mailboxIssue(agentsNode.state, "agents")
	}

	discovered := map[string]bool{}
	if agentsCap != nil {
		names, err := agentsCap.readDirNames()
		if err != nil {
			inventory.AgentsState = MailboxPathUnreadable
			inventory.AgentsIssue = mailboxIssue(MailboxPathUnreadable, "agents")
		} else {
			for _, name := range names {
				discovered[name] = true
			}
		}
	}

	handleSet := make(map[string]bool, len(configured)+len(discovered))
	for handle := range configured {
		handleSet[handle] = true
	}
	for handle := range discovered {
		handleSet[handle] = true
	}
	for _, handle := range additionalHandles {
		handleSet[handle] = true
	}
	handles := make([]string, 0, len(handleSet))
	for handle := range handleSet {
		handles = append(handles, handle)
	}
	sort.Strings(handles)

	plan := mailboxLayoutPlan{
		inventory: inventory,
		nodes:     map[string]mailboxLayoutNode{"agents": agentsNode},
	}
	for _, handle := range handles {
		entry := MailboxInspection{Handle: handle}
		switch {
		case configured[handle] && discovered[handle]:
			entry.Provenance = MailboxConfiguredAndDiscovered
		case configured[handle]:
			entry.Provenance = MailboxConfigured
		default:
			entry.Provenance = MailboxDiscovered
		}
		if err := ValidateHandle(handle); err != nil {
			entry.Status = mailboxSeverity(entry.Provenance)
			entry.Issues = []string{"invalid_handle"}
			plan.inventory.Mailboxes = append(plan.inventory.Mailboxes, entry)
			continue
		}
		inspectOneMailbox(&plan, agentsCap, handle, &entry)
		entry.RepairEligible = configured[handle] &&
			plan.inventory.RepairAuthorized &&
			mailboxIssuesRepairable(entry.Issues)
		plan.inventory.Mailboxes = append(plan.inventory.Mailboxes, entry)
	}
	return plan, nil
}

func mailboxIssuesRepairable(issues []string) bool {
	if len(issues) == 0 {
		return false
	}
	for _, issue := range issues {
		if !strings.HasPrefix(issue, string(MailboxPathMissing)+":") {
			return false
		}
	}
	return true
}

func inspectOneMailbox(plan *mailboxLayoutPlan, agentsCap *layoutDirCapability, handle string, entry *MailboxInspection) {
	caps := map[string]*layoutDirCapability{}
	defer func() {
		for _, cap := range caps {
			cap.close()
		}
	}()

	for _, rel := range mailboxComponentPaths() {
		rootRel := filepath.Join("agents", handle)
		parentRel := ""
		name := handle
		if rel != "." {
			rootRel = filepath.Join(rootRel, rel)
			parentRel = filepath.Dir(rel)
			name = filepath.Base(rel)
		}

		var node mailboxLayoutNode
		var child *layoutDirCapability
		parent := agentsCap
		parentState := plan.inventory.AgentsState
		if rel != "." {
			parent = caps[parentRel]
			parentRootRel := filepath.Join("agents", handle)
			if parentRel != "." {
				parentRootRel = filepath.Join(parentRootRel, parentRel)
			}
			parentState = plan.nodes[parentRootRel].state
		}
		switch {
		case parent != nil:
			node, child = inspectDirectoryComponent(parent, name, rootRel)
		case parentState == MailboxPathMissing:
			node.state = MailboxPathMissing
		default:
			node.state = MailboxPathUnreadable
		}
		plan.nodes[rootRel] = node

		pathResult := MailboxPathInspection{Path: filepath.ToSlash(rel), State: node.state}
		if node.state != MailboxPathMissing && node.mode != 0 {
			pathResult.Mode = fmt.Sprintf("%04o", node.mode.Perm())
		}
		entry.Paths = append(entry.Paths, pathResult)

		if node.state != MailboxPathDirectory {
			entry.Issues = append(entry.Issues, mailboxIssue(node.state, rel))
		} else if !layoutModeSupported(node.mode) {
			entry.Issues = append(entry.Issues, "permissive_mode:"+filepath.ToSlash(rel))
		}

		if child != nil {
			caps[rel] = child
		}
	}
	if len(entry.Issues) == 0 {
		entry.Status = "ok"
	} else {
		entry.Status = mailboxSeverity(entry.Provenance)
	}
}

func mailboxSeverity(provenance MailboxProvenance) string {
	if provenance == MailboxDiscovered {
		return "warn"
	}
	return "error"
}

// InspectMailboxLayout builds the configured+discovered mailbox inventory
// through the pinned root capability without changing the filesystem.
func InspectMailboxLayout(root *DeliveryRoot) (MailboxInventory, error) {
	plan, err := inspectMailboxLayout(root)
	return plan.inventory, err
}

// InspectMailboxLayoutWithAuthorization inventories root using the exact
// retained config capability supplied by the caller. effectiveAgents are
// treated as configured for callers whose roster includes implicit handles.
func InspectMailboxLayoutWithAuthorization(root *DeliveryRoot, authorization *MailboxConfigAuthorization, effectiveAgents ...string) (MailboxInventory, error) {
	if authorization == nil || authorization.pin == nil {
		return MailboxInventory{}, fmt.Errorf("mailbox config authorization is missing")
	}
	if err := authorization.Verify(); err != nil {
		return MailboxInventory{}, fmt.Errorf("verify mailbox config authorization: %w", err)
	}
	cfg, err := mailboxConfigWithEffectiveAgents(authorization.pin.cfg, effectiveAgents)
	if err != nil {
		return MailboxInventory{}, err
	}
	plan, err := inspectMailboxLayoutWithConfig(root, cfg)
	if err != nil {
		return plan.inventory, err
	}
	if err := authorization.Verify(); err != nil {
		return plan.inventory, fmt.Errorf("mailbox config authorization changed during inspection: %w", err)
	}
	return plan.inventory, nil
}

func mailboxConfigWithEffectiveAgents(cfg mailboxActiveConfig, effectiveAgents []string) (mailboxActiveConfig, error) {
	cfg.Agents = append([]string(nil), cfg.Agents...)
	seen := make(map[string]bool, len(cfg.Agents)+len(effectiveAgents))
	for _, handle := range cfg.Agents {
		seen[handle] = true
	}
	for _, handle := range effectiveAgents {
		if err := ValidateHandle(handle); err != nil {
			return mailboxActiveConfig{}, err
		}
		if !seen[handle] {
			seen[handle] = true
			cfg.Agents = append(cfg.Agents, handle)
		}
	}
	return cfg, nil
}

// ValidateExistingMailboxLayout requires the complete mailbox contract for each
// requested handle without creating or changing any path.
func ValidateExistingMailboxLayout(root *DeliveryRoot, handles ...string) error {
	unique := make([]string, 0, len(handles))
	seen := make(map[string]bool, len(handles))
	for _, handle := range handles {
		if err := ValidateHandle(handle); err != nil {
			return err
		}
		if !seen[handle] {
			seen[handle] = true
			unique = append(unique, handle)
		}
	}
	plan, err := inspectMailboxLayout(root, unique...)
	if err != nil {
		return err
	}
	byHandle := make(map[string]MailboxInspection, len(plan.inventory.Mailboxes))
	for _, mailbox := range plan.inventory.Mailboxes {
		byHandle[mailbox.Handle] = mailbox
	}
	for _, handle := range unique {
		mailbox, ok := byHandle[handle]
		if !ok || len(mailbox.Issues) != 0 {
			issues := mailbox.Issues
			if !ok {
				issues = []string{"missing:."}
			}
			return fmt.Errorf("mailbox for %q is incomplete: %s", handle, strings.Join(issues, ","))
		}
	}
	return nil
}

func preflightHazard(plan mailboxLayoutPlan, repairHandles []string) (string, bool) {
	if !plan.inventory.RepairAuthorized {
		return plan.inventory.ActiveConfigIssue, true
	}
	if plan.inventory.AgentsState != MailboxPathDirectory && plan.inventory.AgentsState != MailboxPathMissing {
		return plan.inventory.AgentsIssue, true
	}
	repairSet := make(map[string]bool, len(repairHandles))
	for _, handle := range repairHandles {
		repairSet[handle] = true
	}
	for _, mailbox := range plan.inventory.Mailboxes {
		if !repairSet[mailbox.Handle] {
			continue
		}
		if len(mailbox.Issues) > 0 && !mailboxIssuesRepairable(mailbox.Issues) {
			return mailbox.Handle + ":" + strings.Join(mailbox.Issues, ","), true
		}
		for _, path := range mailbox.Paths {
			switch path.State {
			case MailboxPathDirectory, MailboxPathMissing:
			default:
				return mailbox.Handle + ":" + mailboxIssue(path.State, path.Path), true
			}
		}
	}
	return "", false
}

func repairComponentPaths(repairHandles []string) []string {
	paths := []string{"agents"}
	seen := map[string]bool{"agents": true}
	handles := append([]string(nil), repairHandles...)
	sort.Strings(handles)
	for _, handle := range handles {
		for _, rel := range mailboxComponentPaths() {
			path := filepath.Join("agents", handle)
			if rel != "." {
				path = filepath.Join(path, rel)
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func repairFailure(code, stage, path string, err error, created []string, root *DeliveryRoot, cfg mailboxActiveConfig) MailboxRepairResult {
	plan, inspectErr := inspectMailboxLayoutWithConfig(root, cfg)
	inventory := plan.inventory
	if inspectErr != nil {
		err = fmt.Errorf("%w; re-inspection failed: %v", err, inspectErr)
	}
	mergeCreatedPaths(&inventory, created)
	return MailboxRepairResult{
		Status:       "partial",
		CreatedPaths: append([]string(nil), created...),
		Failure: &MailboxRepairFailure{
			Code:                    code,
			Stage:                   stage,
			Path:                    filepath.ToSlash(path),
			Message:                 err.Error(),
			DurabilityIndeterminate: stage == "child_sync" || stage == "parent_sync",
		},
		Inventory: inventory,
	}
}

func mergeCreatedPaths(inventory *MailboxInventory, created []string) {
	for _, path := range created {
		slash := filepath.ToSlash(path)
		parts := strings.Split(slash, "/")
		if len(parts) < 2 || parts[0] != "agents" {
			continue
		}
		handle := parts[1]
		relative := "."
		if len(parts) > 2 {
			relative = strings.Join(parts[2:], "/")
		}
		for i := range inventory.Mailboxes {
			if inventory.Mailboxes[i].Handle == handle {
				inventory.Mailboxes[i].CreatedPaths = append(inventory.Mailboxes[i].CreatedPaths, relative)
				break
			}
		}
	}
}

func repairMailboxLayoutHandles(root, configRoot *DeliveryRoot, requestedHandles []string, configuredSet bool, hooks mailboxRepairHooks) MailboxRepairResult {
	authorization, inventory, err := OpenMailboxConfigAuthorization(configRoot)
	if err != nil {
		return MailboxRepairResult{
			Status: "failed",
			Failure: &MailboxRepairFailure{
				Code:    "preflight_failed",
				Stage:   "authorization",
				Message: inventory.ActiveConfigIssue,
			},
			Inventory: inventory,
		}
	}
	defer func() { _ = authorization.Close() }()
	return repairMailboxLayoutHandlesAuthorized(root, authorization, requestedHandles, configuredSet, hooks)
}

func repairMailboxLayoutHandlesAuthorized(root *DeliveryRoot, authorization *MailboxConfigAuthorization, requestedHandles []string, configuredSet bool, hooks mailboxRepairHooks) MailboxRepairResult {
	if authorization == nil || authorization.pin == nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "authorization", Message: "mailbox config authorization is missing"},
		}
	}
	return repairMailboxLayoutHandlesAuthorizedWithConfig(
		root,
		authorization,
		requestedHandles,
		configuredSet,
		authorization.pin.cfg,
		hooks,
	)
}

func repairMailboxLayoutHandlesAuthorizedWithConfig(root *DeliveryRoot, authorization *MailboxConfigAuthorization, requestedHandles []string, configuredSet bool, inspectionConfig mailboxActiveConfig, hooks mailboxRepairHooks) MailboxRepairResult {
	if authorization == nil || authorization.pin == nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "authorization", Message: "mailbox config authorization is missing"},
		}
	}
	configPin := authorization.pin
	if err := configPin.verify(); err != nil {
		return MailboxRepairResult{
			Status: "failed",
			Failure: &MailboxRepairFailure{
				Code:    "authorization_changed",
				Stage:   "authorization",
				Path:    "meta/config.json",
				Message: err.Error(),
			},
			Inventory: MailboxInventory{
				ActiveConfigStatus: "changed",
				ActiveConfigIssue:  err.Error(),
				ConfiguredAgents:   append([]string(nil), inspectionConfig.Agents...),
			},
		}
	}
	plan, err := inspectMailboxLayoutWithConfig(root, inspectionConfig, requestedHandles...)
	if err != nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "unsafe_root", Stage: "preflight", Message: err.Error()},
		}
	}
	repairHandles := requestedHandles
	if configuredSet {
		repairHandles = plan.inventory.ConfiguredAgents
	}
	if issue, hazard := preflightHazard(plan, repairHandles); hazard {
		return MailboxRepairResult{
			Status:    "failed",
			Failure:   &MailboxRepairFailure{Code: "preflight_failed", Stage: "preflight", Message: issue},
			Inventory: plan.inventory,
		}
	}
	if hooks.afterPreflight != nil {
		hooks.afterPreflight()
	}
	if err := configPin.verify(); err != nil {
		return MailboxRepairResult{
			Status:    "failed",
			Failure:   &MailboxRepairFailure{Code: "authorization_changed", Stage: "authorization", Path: "meta/config.json", Message: err.Error()},
			Inventory: plan.inventory,
		}
	}

	rootCap, err := openLayoutRootCapability(root)
	if err != nil {
		return repairFailure("unsafe_root", "open", ".", err, nil, root, inspectionConfig)
	}
	defer rootCap.close()
	caps := map[string]*layoutDirCapability{"": rootCap}
	created := []string{}
	defer func() {
		for path, cap := range caps {
			if path != "" {
				cap.close()
			}
		}
	}()

	for _, path := range repairComponentPaths(repairHandles) {
		parentPath := filepath.Dir(path)
		if parentPath == "." {
			parentPath = ""
		}
		parent := caps[parentPath]
		if parent == nil {
			return repairFailure("concurrent_change", "open", path, errLayoutIdentityChanged, created, root, inspectionConfig)
		}
		name := filepath.Base(path)
		before := plan.nodes[path]

		current, statErr := lstatLayoutNode(parent, name)
		switch before.state {
		case MailboxPathMissing:
			if statErr == nil {
				return repairFailure("concurrent_race", "create", path, fmt.Errorf("%s appeared after preflight", path), created, root, inspectionConfig)
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return repairFailure("create_failed", "create", path, statErr, created, root, inspectionConfig)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("mkdir", path); failErr != nil {
					return repairFailure("create_failed", "mkdir", path, failErr, created, root, inspectionConfig)
				}
			}
			if err := mkdirLayoutDirectory(parent, name, 0o700); err != nil {
				code := "create_failed"
				if errors.Is(err, fs.ErrExist) {
					code = "concurrent_race"
				}
				return repairFailure(code, "mkdir", path, err, created, root, inspectionConfig)
			}
			created = append(created, filepath.ToSlash(path))
			current, statErr = lstatLayoutNode(parent, name)
			if statErr != nil {
				return repairFailure("create_failed", "lstat", path, statErr, created, root, inspectionConfig)
			}
			if current.kind != layoutNodeDirectory {
				return repairFailure("concurrent_race", "identity", path, fmt.Errorf("created path is not a directory"), created, root, inspectionConfig)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("open", path); failErr != nil {
					return repairFailure("create_failed", "open", path, failErr, created, root, inspectionConfig)
				}
			}
			child, openErr := openLayoutDirectory(parent, name, current)
			if openErr != nil {
				return repairFailure("create_failed", "open", path, openErr, created, root, inspectionConfig)
			}
			caps[path] = child
			if hooks.fail != nil {
				if failErr := hooks.fail("chmod", path); failErr != nil {
					return repairFailure("create_failed", "chmod", path, failErr, created, root, inspectionConfig)
				}
			}
			if err := child.chmod(0o700); err != nil {
				return repairFailure("create_failed", "chmod", path, err, created, root, inspectionConfig)
			}
			mode, err := child.mode()
			if err != nil {
				return repairFailure("create_failed", "fstat", path, err, created, root, inspectionConfig)
			}
			if !layoutModeSupported(mode) {
				return repairFailure("create_failed", "mode", path, fmt.Errorf("created directory mode is %04o, want 0700", mode.Perm()), created, root, inspectionConfig)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("child_sync", path); failErr != nil {
					return repairFailure("durability_failed", "child_sync", path, failErr, created, root, inspectionConfig)
				}
			}
			if err := child.sync(); err != nil {
				return repairFailure("durability_failed", "child_sync", path, err, created, root, inspectionConfig)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("parent_sync", path); failErr != nil {
					return repairFailure("durability_failed", "parent_sync", path, failErr, created, root, inspectionConfig)
				}
			}
			if err := parent.sync(); err != nil {
				return repairFailure("durability_failed", "parent_sync", path, err, created, root, inspectionConfig)
			}
		case MailboxPathDirectory:
			if statErr != nil || !sameLayoutNode(before.info, current) {
				if statErr == nil {
					statErr = errLayoutIdentityChanged
				}
				return repairFailure("concurrent_change", "identity", path, statErr, created, root, inspectionConfig)
			}
			child, openErr := openLayoutDirectory(parent, name, current)
			if openErr != nil {
				return repairFailure("concurrent_change", "open", path, openErr, created, root, inspectionConfig)
			}
			caps[path] = child
		default:
			return repairFailure("preflight_failed", "preflight", path, fmt.Errorf("unsafe preflight state %s", before.state), created, root, inspectionConfig)
		}
	}

	if err := configPin.verify(); err != nil {
		return repairFailure("authorization_changed", "verify", "meta/config.json", err, created, root, inspectionConfig)
	}
	verifiedPlan, inspectErr := inspectMailboxLayoutWithConfig(root, inspectionConfig, repairHandles...)
	if inspectErr != nil {
		return repairFailure("verification_failed", "verify", ".", inspectErr, created, root, inspectionConfig)
	}
	if hooks.afterFinalInspection != nil {
		hooks.afterFinalInspection()
	}
	inventory := verifiedPlan.inventory
	mergeCreatedPaths(&inventory, created)
	repairSet := make(map[string]bool, len(repairHandles))
	for _, handle := range repairHandles {
		repairSet[handle] = true
	}
	for _, mailbox := range inventory.Mailboxes {
		if !repairSet[mailbox.Handle] {
			continue
		}
		for _, path := range mailbox.Paths {
			switch path.State {
			case MailboxPathDirectory:
			default:
				return repairFailure("verification_failed", "verify", filepath.Join("agents", mailbox.Handle, path.Path), fmt.Errorf("post-repair state %s", path.State), created, root, inspectionConfig)
			}
		}
	}
	if err := authorization.Verify(); err != nil {
		return repairFailure("authorization_changed", "verify", "meta/config.json", err, created, root, inspectionConfig)
	}
	return MailboxRepairResult{
		Status:       "repaired",
		CreatedPaths: append([]string(nil), created...),
		Inventory:    inventory,
	}
}

func repairMailboxLayout(root *DeliveryRoot, hooks mailboxRepairHooks) MailboxRepairResult {
	return repairMailboxLayoutHandles(root, root, nil, true, hooks)
}

// RepairMailboxLayout validates the complete configured set before creating
// any directory and returns exact partial results if creation later fails.
func RepairMailboxLayout(root *DeliveryRoot) MailboxRepairResult {
	return repairMailboxLayout(root, mailboxRepairHooks{})
}

// RepairMailboxLayoutForAgents validates and completes only the requested
// mailbox layouts using the same no-symlink, identity-pinned repair machinery
// as RepairMailboxLayout. A valid active config is still required, but the
// requested handles do not need to be listed in it.
func RepairMailboxLayoutForAgents(root *DeliveryRoot, agents []string) MailboxRepairResult {
	return RepairMailboxLayoutForAgentsAuthorized(root, root, agents)
}

// RepairMailboxLayoutForAgentsAuthorized validates and completes only agents in
// root while taking initialization and roster authority from configRoot.
func RepairMailboxLayoutForAgentsAuthorized(root, configRoot *DeliveryRoot, agents []string) MailboxRepairResult {
	authorization, inventory, err := OpenMailboxConfigAuthorization(configRoot)
	if err != nil {
		return MailboxRepairResult{
			Status:    "failed",
			Failure:   &MailboxRepairFailure{Code: "preflight_failed", Stage: "authorization", Message: inventory.ActiveConfigIssue},
			Inventory: inventory,
		}
	}
	defer func() { _ = authorization.Close() }()
	return RepairMailboxLayoutForAgentsWithAuthorization(root, authorization, agents)
}

// RepairMailboxLayoutForAgentsWithAuthorization repairs only agents using the
// exact retained config authorization previously used for roster validation.
func RepairMailboxLayoutForAgentsWithAuthorization(root *DeliveryRoot, authorization *MailboxConfigAuthorization, agents []string) MailboxRepairResult {
	handles := make([]string, 0, len(agents))
	seen := make(map[string]bool, len(agents))
	for _, handle := range agents {
		if err := ValidateHandle(handle); err != nil {
			return MailboxRepairResult{
				Status:  "failed",
				Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "preflight", Message: err.Error()},
			}
		}
		if !seen[handle] {
			seen[handle] = true
			handles = append(handles, handle)
		}
	}
	return repairMailboxLayoutHandlesAuthorized(root, authorization, handles, false, mailboxRepairHooks{})
}

// RepairMailboxLayoutForAgentsWithAuthorizationAndWriteGuard is the
// lease-bound variant used by launch Apply. writeGuard is revalidated before
// each directory mutation and durability sync; a revoked authority therefore
// stops the repair at the next owning write boundary.
func RepairMailboxLayoutForAgentsWithAuthorizationAndWriteGuard(root *DeliveryRoot, authorization *MailboxConfigAuthorization, agents []string, writeGuard func() error) MailboxRepairResult {
	if authorization == nil || authorization.pin == nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "authorization", Message: "mailbox config authorization is missing"},
		}
	}
	if writeGuard == nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "authorization", Message: "mailbox write guard is missing"},
		}
	}
	return repairMailboxLayoutHandlesAuthorizedWithConfig(
		root,
		authorization,
		agents,
		false,
		authorization.pin.cfg,
		mailboxRepairHooks{fail: func(stage, _ string) error {
			switch stage {
			case "mkdir", "chmod", "child_sync", "parent_sync":
				return writeGuard()
			default:
				return nil
			}
		}},
	)
}

// RepairMailboxLayoutForConfiguredAgentsWithAuthorization repairs an effective
// configured roster, including caller-owned implicit handles, while retaining
// the exact on-disk config authorization for mutation checks.
func RepairMailboxLayoutForConfiguredAgentsWithAuthorization(root *DeliveryRoot, authorization *MailboxConfigAuthorization, effectiveAgents []string) MailboxRepairResult {
	if authorization == nil || authorization.pin == nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "authorization", Message: "mailbox config authorization is missing"},
		}
	}
	cfg, err := mailboxConfigWithEffectiveAgents(authorization.pin.cfg, effectiveAgents)
	if err != nil {
		return MailboxRepairResult{
			Status:  "failed",
			Failure: &MailboxRepairFailure{Code: "preflight_failed", Stage: "preflight", Message: err.Error()},
		}
	}
	return repairMailboxLayoutHandlesAuthorizedWithConfig(
		root,
		authorization,
		cfg.Agents,
		false,
		cfg,
		mailboxRepairHooks{},
	)
}
