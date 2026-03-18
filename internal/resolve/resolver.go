// internal/resolve/resolver.go
package resolve

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/discover"
	"github.com/avivsinai/agent-message-queue/internal/metadata"
)

// Target represents a resolved delivery target.
type Target struct {
	Agent       string
	Session     string
	SessionRoot string // absolute path to the session root for delivery
	Project     string
}

// InboxPath returns the absolute path to the target's inbox directory.
func (t Target) InboxPath() string {
	return filepath.Join(t.SessionRoot, "agents", t.Agent, "inbox")
}

// Resolver resolves Endpoint addresses to concrete delivery targets.
type Resolver struct {
	SessionRoot    string   // current session root (AM_ROOT)
	BaseRoot       string   // AMQ base root (from .amqrc)
	ProjectDir     string   // current project directory
	DiscoveryRoots []string // directories to scan for other projects
}

// NewResolver creates a resolver with the given context.
func NewResolver(sessionRoot, baseRoot, projectDir string) *Resolver {
	return &Resolver{
		SessionRoot: sessionRoot,
		BaseRoot:    baseRoot,
		ProjectDir:  projectDir,
	}
}

// Resolve resolves an Endpoint to one or more delivery Targets.
func (r *Resolver) Resolve(ep Endpoint) ([]Target, error) {
	switch ep.Kind {
	case KindAgent:
		return r.resolveAgent(ep)
	case KindChannel:
		return r.resolveChannel(ep)
	default:
		return nil, fmt.Errorf("unknown endpoint kind: %q", ep.Kind)
	}
}

func (r *Resolver) resolveAgent(ep Endpoint) ([]Target, error) {
	// Local: bare handle in current session
	if ep.IsLocal() {
		return r.resolveLocalAgent(ep.Agent)
	}

	// Cross-project: explicit project qualifier
	if ep.IsCrossProject() {
		return r.resolveCrossProjectAgent(ep)
	}

	// Has a Session field but no Project field.
	// This could be:
	//   1. An explicit cross-session address (agent@session/X)
	//   2. An ambiguous agent@name (could be session or project)
	//
	// For ambiguous forms, we check local sessions first, then discovery.
	// If both match, we error with disambiguation instructions.
	return r.resolveAmbiguousOrCrossSession(ep)
}

func (r *Resolver) resolveLocalAgent(agent string) ([]Target, error) {
	inbox := filepath.Join(r.SessionRoot, "agents", agent, "inbox")
	if err := verifyMailbox(inbox); err != nil {
		return nil, fmt.Errorf("agent %q not found in current session: %w", agent, err)
	}
	return []Target{{
		Agent:       agent,
		Session:     filepath.Base(r.SessionRoot),
		SessionRoot: r.SessionRoot,
	}}, nil
}

// resolveAmbiguousOrCrossSession handles agent@name where name could be a
// session (same project) or a project slug (cross-project). We check local
// sessions first, then the discovery cache. If both match, we error.
func (r *Resolver) resolveAmbiguousOrCrossSession(ep Endpoint) ([]Target, error) {
	name := ep.Session // the ambiguous qualifier

	// Check if local session exists with that name
	sessRoot := filepath.Join(r.BaseRoot, name)
	inbox := filepath.Join(sessRoot, "agents", ep.Agent, "inbox")
	localFound := verifyMailbox(inbox) == nil

	// Check if a project with that slug exists
	projectFound := false
	var proj discover.Project
	projResult, err := r.findProject(name)
	if err == nil {
		// Verify the project is not the current project (that would be a session ref)
		canonProj, _ := filepath.EvalSymlinks(projResult.Dir)
		canonCurr, _ := filepath.EvalSymlinks(r.ProjectDir)
		if canonProj != canonCurr {
			projectFound = true
			proj = projResult
		}
	}

	// Both match: ambiguous, error with disambiguation instructions
	if localFound && projectFound {
		return nil, fmt.Errorf(
			"ambiguous address %q: matches both session %q in current project and project %q; "+
				"use %s@session/%s for the session or %s@project/%s for the project",
			ep.String(), name, name, ep.Agent, name, ep.Agent, name,
		)
	}

	// Local session match
	if localFound {
		return []Target{{
			Agent:       ep.Agent,
			Session:     name,
			SessionRoot: sessRoot,
		}}, nil
	}

	// Project match: search for agent in that project
	if projectFound {
		return r.resolveAgentInProject(ep.Agent, name, proj, "")
	}

	// Neither match
	return nil, fmt.Errorf("agent %q not found in session %q (and no project %q found)", ep.Agent, name, name)
}

func (r *Resolver) resolveCrossProjectAgent(ep Endpoint) ([]Target, error) {
	proj, err := r.findProject(ep.Project)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", ep.Project, err)
	}

	// Verify same-user ownership
	if err := verifySameOwner(r.ProjectDir, proj.Dir); err != nil {
		return nil, fmt.Errorf("cross-project delivery to %q denied: %w", ep.Project, err)
	}

	return r.resolveAgentInProject(ep.Agent, ep.Project, proj, ep.Session)
}

// resolveAgentInProject finds an agent within a discovered project.
// If session is specified, only that session is checked. Otherwise, all
// sessions are searched and the match must be unique.
func (r *Resolver) resolveAgentInProject(agent, projectSlug string, proj discover.Project, session string) ([]Target, error) {
	if session != "" {
		// Explicit session
		sessRoot := filepath.Join(proj.BaseRoot, session)
		inbox := filepath.Join(sessRoot, "agents", agent, "inbox")
		if err := verifyMailbox(inbox); err != nil {
			return nil, fmt.Errorf("agent %q not found in %s:%s", agent, projectSlug, session)
		}
		return []Target{{
			Agent:       agent,
			Session:     session,
			SessionRoot: sessRoot,
			Project:     projectSlug,
		}}, nil
	}

	// Search all sessions for the agent (must be unique)
	var matches []Target
	for _, sess := range proj.Sessions {
		for _, a := range sess.Agents {
			if a == agent {
				inbox := filepath.Join(sess.Root, "agents", agent, "inbox")
				if verifyMailbox(inbox) == nil {
					matches = append(matches, Target{
						Agent:       agent,
						Session:     sess.Name,
						SessionRoot: sess.Root,
						Project:     projectSlug,
					})
				}
			}
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("agent %q not found in project %q", agent, projectSlug)
	case 1:
		return matches, nil
	default:
		sessions := make([]string, len(matches))
		for i, m := range matches {
			sessions[i] = m.Session
		}
		return nil, fmt.Errorf("agent %q is ambiguous in project %q (found in sessions: %v); use %s@%s:<session>",
			agent, projectSlug, sessions, agent, projectSlug)
	}
}

func (r *Resolver) resolveChannel(ep Endpoint) ([]Target, error) {
	baseRoot := r.BaseRoot
	projectSlug := ""

	// Cross-project channel
	if ep.IsCrossProject() {
		proj, err := r.findProject(ep.Project)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", ep.Project, err)
		}
		if err := verifySameOwner(r.ProjectDir, proj.Dir); err != nil {
			return nil, fmt.Errorf("cross-project channel %q denied: %w", ep.String(), err)
		}
		baseRoot = proj.BaseRoot
		projectSlug = ep.Project
	}

	proj, err := discover.DiscoverProject(filepath.Dir(baseRoot))
	if err != nil {
		// Fallback: scan sessions directly from the base root
		return r.resolveChannelFromBase(baseRoot, ep, projectSlug)
	}

	return r.resolveChannelFromProject(proj, ep, projectSlug)
}

func (r *Resolver) resolveChannelFromBase(baseRoot string, ep Endpoint, project string) ([]Target, error) {
	sessions, _ := scanSessionsRaw(baseRoot)
	return r.expandChannel(sessions, ep, project)
}

func (r *Resolver) resolveChannelFromProject(proj discover.Project, ep Endpoint, project string) ([]Target, error) {
	return r.expandChannel(proj.Sessions, ep, project)
}

func (r *Resolver) expandChannel(sessions []discover.Session, ep Endpoint, project string) ([]Target, error) {
	var targets []Target
	seen := make(map[string]bool) // dedup by inbox path

	// Custom channels (not "all", not "session") require agent.json membership check.
	isCustomChannel := ep.Channel != "all" && ep.Channel != "session"

	for _, sess := range sessions {
		// #session/X: only agents in that session
		if ep.Channel == "session" && ep.Session != "" {
			if sess.Name != ep.Session {
				continue
			}
		}

		for _, agent := range sess.Agents {
			inbox := filepath.Join(sess.Root, "agents", agent, "inbox")
			if err := verifyMailbox(inbox); err != nil {
				continue // skip agents without valid mailboxes
			}

			// For custom channels, check agent.json channel membership
			if isCustomChannel {
				agentJSONPath := filepath.Join(sess.Root, "agents", agent, "agent.json")
				meta, err := metadata.ReadAgentMeta(agentJSONPath)
				if err != nil {
					continue // no agent.json or unreadable → not subscribed
				}
				if !hasChannel(meta.Channels, ep.Channel) {
					continue // agent not subscribed to this channel
				}
			}

			// Dedup by canonical inbox path to prevent double-delivery
			canonical, err := filepath.Abs(inbox)
			if err != nil {
				canonical = inbox
			}
			if seen[canonical] {
				continue
			}
			seen[canonical] = true

			targets = append(targets, Target{
				Agent:       agent,
				Session:     sess.Name,
				SessionRoot: sess.Root,
				Project:     project,
			})
		}
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("channel %s resolved to zero targets", ep.String())
	}
	return targets, nil
}

// hasChannel checks if a channel name is in the given list.
func hasChannel(channels []string, name string) bool {
	for _, ch := range channels {
		if ch == name {
			return true
		}
	}
	return false
}

func (r *Resolver) findProject(slug string) (discover.Project, error) {
	// Try discovery cache first
	cachePath := discover.DefaultCachePath()
	cache, _ := discover.LoadCache(cachePath)

	// Check cache for duplicates before using cached result
	cacheMatches := cache.FindAllBySlug(slug)
	validCacheMatches := make([]discover.CacheEntry, 0, len(cacheMatches))
	for _, e := range cacheMatches {
		if e.Validate() {
			validCacheMatches = append(validCacheMatches, e)
		}
	}
	if len(validCacheMatches) > 1 {
		paths := make([]string, len(validCacheMatches))
		for i, e := range validCacheMatches {
			paths[i] = e.Dir
		}
		return discover.Project{}, fmt.Errorf(
			"ambiguous slug %q: multiple projects share this name (%v); use project_id in .amqrc to disambiguate",
			slug, paths,
		)
	}
	if len(validCacheMatches) == 1 {
		proj, err := discover.DiscoverProject(validCacheMatches[0].Dir)
		if err == nil {
			return proj, nil
		}
	}

	// Scan discovery roots
	roots := r.DiscoveryRoots
	if len(roots) == 0 {
		// Default: parent of current project
		roots = []string{filepath.Dir(r.ProjectDir)}
	}

	projects, err := discover.ScanProjects(roots, 2)
	if err != nil {
		return discover.Project{}, fmt.Errorf("discovery scan: %w", err)
	}

	// Update cache
	for _, p := range projects {
		cache.Update(p)
	}
	_ = discover.SaveCache(cachePath, cache)

	// Find by slug, detecting duplicates
	var matches []discover.Project
	for _, p := range projects {
		if p.Slug == slug {
			matches = append(matches, p)
		}
	}

	switch len(matches) {
	case 0:
		return discover.Project{}, fmt.Errorf("not found")
	case 1:
		return matches[0], nil
	default:
		paths := make([]string, len(matches))
		for i, m := range matches {
			paths[i] = m.Dir
		}
		return discover.Project{}, fmt.Errorf(
			"ambiguous slug %q: multiple projects share this name (%v); use project_id in .amqrc to disambiguate",
			slug, paths,
		)
	}
}

// scanSessionsRaw scans a base root for sessions without loading a full project.
func scanSessionsRaw(baseRoot string) ([]discover.Session, error) {
	entries, err := os.ReadDir(baseRoot)
	if err != nil {
		return nil, err
	}
	var sessions []discover.Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		agentsDir := filepath.Join(baseRoot, e.Name(), "agents")
		agentEntries, err := os.ReadDir(agentsDir)
		if err != nil {
			continue
		}
		var agents []string
		for _, ae := range agentEntries {
			if ae.IsDir() {
				agents = append(agents, ae.Name())
			}
		}
		if len(agents) > 0 {
			sessions = append(sessions, discover.Session{
				Name:   e.Name(),
				Root:   filepath.Join(baseRoot, e.Name()),
				Agents: agents,
			})
		}
	}
	return sessions, nil
}

// verifyMailbox checks that a mailbox inbox directory exists on disk.
// The resolver MUST verify existence before returning a target; it must
// never rely on MkdirAll in the delivery path to auto-create mailboxes
// for federated (cross-session/cross-project) targets.
func verifyMailbox(inbox string) error {
	info, err := os.Stat(inbox)
	if err != nil {
		return fmt.Errorf("mailbox not found: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mailbox path is not a directory: %s", inbox)
	}
	return nil
}

// verifySameOwner checks that both paths are owned by the same OS user.
// This prevents cross-user delivery through path traversal or symlink
// manipulation. Returns nil if both paths have the same uid.
func verifySameOwner(pathA, pathB string) error {
	// Canonicalize to resolve symlinks
	canonA, err := filepath.EvalSymlinks(pathA)
	if err != nil {
		return fmt.Errorf("canonicalize %q: %w", pathA, err)
	}
	canonB, err := filepath.EvalSymlinks(pathB)
	if err != nil {
		return fmt.Errorf("canonicalize %q: %w", pathB, err)
	}

	infoA, err := os.Stat(canonA)
	if err != nil {
		return fmt.Errorf("stat %q: %w", canonA, err)
	}
	infoB, err := os.Stat(canonB)
	if err != nil {
		return fmt.Errorf("stat %q: %w", canonB, err)
	}

	sysA, ok := infoA.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot get uid for %q: unsupported platform", canonA)
	}
	sysB, ok := infoB.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot get uid for %q: unsupported platform", canonB)
	}

	if sysA.Uid != sysB.Uid {
		return fmt.Errorf("owner mismatch: %q (uid %d) vs %q (uid %d)", canonA, sysA.Uid, canonB, sysB.Uid)
	}
	return nil
}
