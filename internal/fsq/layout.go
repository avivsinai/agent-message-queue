package fsq

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// handleRe matches valid agent handles: lowercase letters, digits, underscore, hyphen.
var handleRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidateHandle returns an error if the agent handle contains path traversal
// characters or does not match the allowed pattern.
func ValidateHandle(agent string) error {
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
	afterPreflight func()
	fail           func(stage, path string) error
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

func repairFailure(code, stage, path string, err error, created []string, root *DeliveryRoot) MailboxRepairResult {
	inventory, inspectErr := InspectMailboxLayout(root)
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

func repairMailboxLayoutHandles(root *DeliveryRoot, requestedHandles []string, configuredSet bool, hooks mailboxRepairHooks) MailboxRepairResult {
	plan, err := inspectMailboxLayout(root, requestedHandles...)
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

	rootCap, err := openLayoutRootCapability(root)
	if err != nil {
		return repairFailure("unsafe_root", "open", ".", err, nil, root)
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
			return repairFailure("concurrent_change", "open", path, errLayoutIdentityChanged, created, root)
		}
		name := filepath.Base(path)
		before := plan.nodes[path]

		current, statErr := lstatLayoutNode(parent, name)
		switch before.state {
		case MailboxPathMissing:
			if statErr == nil {
				return repairFailure("concurrent_race", "create", path, fmt.Errorf("%s appeared after preflight", path), created, root)
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return repairFailure("create_failed", "create", path, statErr, created, root)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("mkdir", path); failErr != nil {
					return repairFailure("create_failed", "mkdir", path, failErr, created, root)
				}
			}
			if err := mkdirLayoutDirectory(parent, name, 0o700); err != nil {
				code := "create_failed"
				if errors.Is(err, fs.ErrExist) {
					code = "concurrent_race"
				}
				return repairFailure(code, "mkdir", path, err, created, root)
			}
			created = append(created, filepath.ToSlash(path))
			current, statErr = lstatLayoutNode(parent, name)
			if statErr != nil {
				return repairFailure("create_failed", "lstat", path, statErr, created, root)
			}
			if current.kind != layoutNodeDirectory {
				return repairFailure("concurrent_race", "identity", path, fmt.Errorf("created path is not a directory"), created, root)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("open", path); failErr != nil {
					return repairFailure("create_failed", "open", path, failErr, created, root)
				}
			}
			child, openErr := openLayoutDirectory(parent, name, current)
			if openErr != nil {
				return repairFailure("create_failed", "open", path, openErr, created, root)
			}
			caps[path] = child
			if hooks.fail != nil {
				if failErr := hooks.fail("chmod", path); failErr != nil {
					return repairFailure("create_failed", "chmod", path, failErr, created, root)
				}
			}
			if err := child.chmod(0o700); err != nil {
				return repairFailure("create_failed", "chmod", path, err, created, root)
			}
			mode, err := child.mode()
			if err != nil {
				return repairFailure("create_failed", "fstat", path, err, created, root)
			}
			if !layoutModeSupported(mode) {
				return repairFailure("create_failed", "mode", path, fmt.Errorf("created directory mode is %04o, want 0700", mode.Perm()), created, root)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("child_sync", path); failErr != nil {
					return repairFailure("durability_failed", "child_sync", path, failErr, created, root)
				}
			}
			if err := child.sync(); err != nil {
				return repairFailure("durability_failed", "child_sync", path, err, created, root)
			}
			if hooks.fail != nil {
				if failErr := hooks.fail("parent_sync", path); failErr != nil {
					return repairFailure("durability_failed", "parent_sync", path, failErr, created, root)
				}
			}
			if err := parent.sync(); err != nil {
				return repairFailure("durability_failed", "parent_sync", path, err, created, root)
			}
		case MailboxPathDirectory:
			if statErr != nil || !sameLayoutNode(before.info, current) {
				if statErr == nil {
					statErr = errLayoutIdentityChanged
				}
				return repairFailure("concurrent_change", "identity", path, statErr, created, root)
			}
			child, openErr := openLayoutDirectory(parent, name, current)
			if openErr != nil {
				return repairFailure("concurrent_change", "open", path, openErr, created, root)
			}
			caps[path] = child
		default:
			return repairFailure("preflight_failed", "preflight", path, fmt.Errorf("unsafe preflight state %s", before.state), created, root)
		}
	}

	verifiedPlan, inspectErr := inspectMailboxLayout(root, repairHandles...)
	if inspectErr != nil {
		return repairFailure("verification_failed", "verify", ".", inspectErr, created, root)
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
				return repairFailure("verification_failed", "verify", filepath.Join("agents", mailbox.Handle, path.Path), fmt.Errorf("post-repair state %s", path.State), created, root)
			}
		}
	}
	return MailboxRepairResult{
		Status:       "repaired",
		CreatedPaths: append([]string(nil), created...),
		Inventory:    inventory,
	}
}

func repairMailboxLayout(root *DeliveryRoot, hooks mailboxRepairHooks) MailboxRepairResult {
	return repairMailboxLayoutHandles(root, nil, true, hooks)
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
	return repairMailboxLayoutHandles(root, handles, false, mailboxRepairHooks{})
}
