# AMQ Federation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable cross-session and cross-project messaging between AMQ agent pairs, with convention-based discovery and decentralized channel fan-out.

**Architecture:** Extend AMQ's existing Maildir delivery to support qualified addresses (`agent@session`, `agent@project:session`). Discovery scans `.amqrc` files by convention. Channels are fan-out aliases over normal inbox deliveries. Session/agent metadata files provide presence and capability advertisement. No daemon, no broker, no shared queues.

**Tech Stack:** Go 1.25+, existing AMQ Maildir primitives, JSON metadata files.

**Spec:** `docs/gpt54-pro-federation-design.md` (GPT-5.4-Pro Extended design), project CLAUDE.md (architecture reference).

**Worktree:** `.claude/worktrees/federation` (branch: `feature/federation`)

---

## File Structure

### New packages

| File | Responsibility |
|------|---------------|
| `internal/resolve/address.go` | Parse qualified addresses (`agent@session`, `agent@project:session`, `#channel`) |
| `internal/resolve/address_test.go` | Address parsing tests |
| `internal/resolve/resolver.go` | Resolve parsed addresses to concrete Maildir paths |
| `internal/resolve/resolver_test.go` | Resolver tests with temp directory fixtures |
| `internal/discover/discover.go` | Scan filesystem for `.amqrc` files, enumerate sessions/agents |
| `internal/discover/discover_test.go` | Discovery tests |
| `internal/discover/cache.go` | Read/write `~/.cache/amq/discovery-v1.json` (non-authoritative cache) |
| `internal/discover/cache_test.go` | Cache tests |
| `internal/metadata/session.go` | Read/write `session.json` per session |
| `internal/metadata/agent.go` | Read/write `agent.json` per agent |
| `internal/metadata/metadata_test.go` | Metadata tests |

### Modified files

| File | Changes |
|------|---------|
| `internal/format/message.go` | Add `Origin` and `Delivery` structs to Header, bump schema to 2 |
| `internal/format/message_test.go` | Tests for schema 2, backward compat with schema 1 |
| `internal/fsq/layout.go` | Add `SessionJSON()`, `AgentJSON()` path helpers |
| `internal/cli/common.go` | Add `AM_PROJECT`, `AM_SESSION`, `AM_BASE_ROOT` env var constants |
| `internal/cli/send.go` | Integrate resolver for qualified `--to` addresses |
| `internal/cli/reply.go` | Use `origin.reply_to` for cross-session replies |
| `internal/cli/env.go` | Extend `.amqrc` with optional `project`/`project_id` fields, emit new env vars |
| `internal/cli/coop.go` | Write `session.json`/`agent.json` on init |
| `internal/cli/coop_exec_unix.go` | Set `AM_PROJECT`, `AM_SESSION`, `AM_BASE_ROOT`, write metadata, add `--topic`/`--claim`/`--channel` flags |
| `internal/cli/cli.go` | Register new commands: `discover`, `who`, `resolve`, `channel`, `announce` |
| `internal/cli/discover.go` | New: `amq discover` command |
| `internal/cli/who.go` | New: `amq who` command |
| `internal/cli/resolve_cmd.go` | New: `amq resolve` command |
| `internal/cli/channel.go` | New: `amq channel join/leave/list` command |
| `internal/cli/announce.go` | New: `amq announce` command (channel fan-out sugar) |

---

## Task 1: Address Parser

Parse qualified AMQ addresses into a structured `Endpoint` type. No I/O — pure string parsing.

**Files:**
- Create: `internal/resolve/address.go`
- Create: `internal/resolve/address_test.go`

- [ ] **Step 1: Write failing tests for address parsing**

```go
// internal/resolve/address_test.go
package resolve

import (
	"testing"
)

func TestParseAddress_BareHandle(t *testing.T) {
	ep, err := ParseAddress("codex")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "codex" || ep.Session != "" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_AgentAtSession(t *testing.T) {
	ep, err := ParseAddress("codex@auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "codex" || ep.Session != "auth" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_AgentAtProjectSession(t *testing.T) {
	ep, err := ParseAddress("claude@infra-lib:auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "claude" || ep.Project != "infra-lib" || ep.Session != "auth" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_AgentAtProject(t *testing.T) {
	ep, err := ParseAddress("claude@infra-lib")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "claude" || ep.Project != "infra-lib" || ep.Session != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_Channel(t *testing.T) {
	ep, err := ParseAddress("#events")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindChannel || ep.Channel != "events" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_ChannelAtProject(t *testing.T) {
	ep, err := ParseAddress("#all@infra-lib")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindChannel || ep.Channel != "all" || ep.Project != "infra-lib" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_SessionChannel(t *testing.T) {
	ep, err := ParseAddress("#session/auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindChannel || ep.Channel != "session" || ep.Session != "auth" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_ExplicitLongForm(t *testing.T) {
	ep, err := ParseAddress("claude@session/auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Agent != "claude" || ep.Session != "auth" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_ExplicitProjectLongForm(t *testing.T) {
	ep, err := ParseAddress("claude@project/infra-lib/session/auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Agent != "claude" || ep.Project != "infra-lib" || ep.Session != "auth" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_Invalid(t *testing.T) {
	cases := []string{"", "@auth", "#", "claude@", "claude@@auth", "UPPER"}
	for _, c := range cases {
		if _, err := ParseAddress(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestEndpoint_IsLocal(t *testing.T) {
	local, _ := ParseAddress("codex")
	if !local.IsLocal() {
		t.Fatal("bare handle should be local")
	}
	cross, _ := ParseAddress("codex@auth")
	if cross.IsLocal() {
		t.Fatal("qualified address should not be local")
	}
}

func TestEndpoint_IsCrossProject(t *testing.T) {
	ep, _ := ParseAddress("claude@infra-lib:auth")
	if !ep.IsCrossProject() {
		t.Fatal("should be cross-project")
	}
	ep2, _ := ParseAddress("codex@auth")
	if ep2.IsCrossProject() {
		t.Fatal("session-only should not be cross-project")
	}
}

func TestEndpoint_String(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"codex", "codex"},
		{"codex@auth", "codex@auth"},
		{"claude@infra-lib:auth", "claude@infra-lib:auth"},
		{"#events", "#events"},
		{"#all@infra-lib", "#all@infra-lib"},
	}
	for _, c := range cases {
		ep, err := ParseAddress(c.input)
		if err != nil {
			t.Fatal(err)
		}
		if ep.String() != c.want {
			t.Errorf("String(%q) = %q, want %q", c.input, ep.String(), c.want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd .claude/worktrees/federation && go test ./internal/resolve/ -v`
Expected: compilation error (package doesn't exist)

- [ ] **Step 3: Implement address parser**

```go
// internal/resolve/address.go
package resolve

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	KindAgent   = "agent"
	KindChannel = "channel"
)

// Endpoint is the canonical internal representation of a qualified AMQ address.
type Endpoint struct {
	Kind    string // "agent" or "channel"
	Agent   string // for agent endpoints
	Channel string // for channel endpoints
	Session string // optional session qualifier
	Project string // optional project qualifier
}

var handleRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ParseAddress parses a user-facing AMQ address string into an Endpoint.
//
// Supported forms:
//   codex                           → local agent
//   codex@auth                      → agent in session "auth" (same project)
//   claude@infra-lib:auth           → agent in project "infra-lib", session "auth"
//   claude@infra-lib                → agent in project "infra-lib" (session unspecified)
//   claude@session/auth             → explicit: agent in session "auth"
//   claude@project/infra-lib        → explicit: agent in project "infra-lib"
//   claude@project/infra-lib/session/auth → explicit: project + session
//   #events                         → channel in current project
//   #all@infra-lib                  → channel in project "infra-lib"
//   #session/auth                   → channel scoped to session "auth"
//   #session/auth@infra-lib         → channel scoped to session in project
func ParseAddress(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty address")
	}

	// Channel addresses start with #
	if strings.HasPrefix(raw, "#") {
		return parseChannel(raw[1:])
	}

	return parseAgent(raw)
}

func parseAgent(raw string) (Endpoint, error) {
	// Split on @ to separate agent from qualifier
	parts := strings.SplitN(raw, "@", 2)
	agent := parts[0]
	if !handleRe.MatchString(agent) {
		return Endpoint{}, fmt.Errorf("invalid agent handle %q: must be lowercase alphanumeric, dash, or underscore", agent)
	}

	ep := Endpoint{Kind: KindAgent, Agent: agent}

	if len(parts) == 1 {
		return ep, nil // bare handle
	}

	qualifier := parts[1]
	if qualifier == "" {
		return Endpoint{}, fmt.Errorf("empty qualifier after @")
	}

	// Explicit long forms: session/X, project/X, project/X/session/Y
	if strings.HasPrefix(qualifier, "session/") {
		ep.Session = qualifier[len("session/"):]
		if !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q", ep.Session)
		}
		return ep, nil
	}
	if strings.HasPrefix(qualifier, "project/") {
		rest := qualifier[len("project/"):]
		if idx := strings.Index(rest, "/session/"); idx >= 0 {
			ep.Project = rest[:idx]
			ep.Session = rest[idx+len("/session/"):]
		} else {
			ep.Project = rest
		}
		if !handleRe.MatchString(ep.Project) {
			return Endpoint{}, fmt.Errorf("invalid project name %q", ep.Project)
		}
		if ep.Session != "" && !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q", ep.Session)
		}
		return ep, nil
	}

	// Short forms: agent@qualifier or agent@project:session
	if strings.Contains(qualifier, ":") {
		colonParts := strings.SplitN(qualifier, ":", 2)
		ep.Project = colonParts[0]
		ep.Session = colonParts[1]
		if !handleRe.MatchString(ep.Project) {
			return Endpoint{}, fmt.Errorf("invalid project name %q", ep.Project)
		}
		if !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q", ep.Session)
		}
		return ep, nil
	}

	// agent@name — ambiguous: could be session or project.
	// Stored as-is; the resolver disambiguates at resolution time.
	// For now, store in Session (local-first resolution).
	// The resolver will check local sessions first, then project registry.
	if !handleRe.MatchString(qualifier) {
		return Endpoint{}, fmt.Errorf("invalid qualifier %q", qualifier)
	}
	ep.Session = qualifier
	return ep, nil
}

func parseChannel(raw string) (Endpoint, error) {
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty channel name after #")
	}

	ep := Endpoint{Kind: KindChannel}

	// Check for project qualifier: #channel@project
	if idx := strings.Index(raw, "@"); idx >= 0 {
		channelPart := raw[:idx]
		projectPart := raw[idx+1:]
		if !handleRe.MatchString(projectPart) {
			return Endpoint{}, fmt.Errorf("invalid project name %q in channel address", projectPart)
		}
		// Channel part may contain session/ prefix
		ep.Project = projectPart
		raw = channelPart
	}

	// Check for session-scoped channel: #session/auth
	if strings.HasPrefix(raw, "session/") {
		ep.Channel = "session"
		ep.Session = raw[len("session/"):]
		if !handleRe.MatchString(ep.Session) {
			return Endpoint{}, fmt.Errorf("invalid session name %q in channel", ep.Session)
		}
		return ep, nil
	}

	if !handleRe.MatchString(raw) {
		return Endpoint{}, fmt.Errorf("invalid channel name %q", raw)
	}
	ep.Channel = raw
	return ep, nil
}

// IsLocal returns true if the address targets the current session only.
func (e Endpoint) IsLocal() bool {
	return e.Session == "" && e.Project == ""
}

// IsCrossProject returns true if the address targets a different project.
func (e Endpoint) IsCrossProject() bool {
	return e.Project != ""
}

// IsCrossSession returns true if the address targets a different session (same project).
func (e Endpoint) IsCrossSession() bool {
	return e.Session != "" && e.Project == ""
}

// String returns the canonical string form of the endpoint.
func (e Endpoint) String() string {
	switch e.Kind {
	case KindChannel:
		s := "#" + e.Channel
		if e.Session != "" {
			s = "#" + e.Channel + "/" + e.Session
		}
		if e.Project != "" {
			s += "@" + e.Project
		}
		return s
	default: // agent
		s := e.Agent
		if e.Project != "" && e.Session != "" {
			s += "@" + e.Project + ":" + e.Session
		} else if e.Project != "" {
			s += "@" + e.Project
		} else if e.Session != "" {
			s += "@" + e.Session
		}
		return s
	}
}
```

- [ ] **Step 4: Run tests and verify they pass**

Run: `cd .claude/worktrees/federation && go test ./internal/resolve/ -v`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
cd .claude/worktrees/federation
git add internal/resolve/
git commit -m "feat: add address parser for qualified AMQ addresses

Supports bare handles, session-qualified (agent@session),
project-qualified (agent@project:session), channel (#events),
and explicit long forms (agent@project/X/session/Y).

Pure string parsing, no I/O."
```

---

## Task 2: Project Discovery

Scan filesystem for `.amqrc` files, enumerate sessions and agents. Discovery cache as optimization only.

**Files:**
- Create: `internal/discover/discover.go`
- Create: `internal/discover/discover_test.go`
- Create: `internal/discover/cache.go`
- Create: `internal/discover/cache_test.go`

- [ ] **Step 1: Write failing tests for project discovery**

```go
// internal/discover/discover_test.go
package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setupProject(t *testing.T, base, name, root string, sessions []string) string {
	t.Helper()
	projDir := filepath.Join(base, name)
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	amqrc := map[string]string{"root": root}
	data, _ := json.Marshal(amqrc)
	if err := os.WriteFile(filepath.Join(projDir, ".amqrc"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	amqRoot := filepath.Join(projDir, root)
	for _, sess := range sessions {
		agentDir := filepath.Join(amqRoot, sess, "agents", "claude", "inbox", "new")
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			t.Fatal(err)
		}
		agentDir2 := filepath.Join(amqRoot, sess, "agents", "codex", "inbox", "new")
		if err := os.MkdirAll(agentDir2, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return projDir
}

func TestDiscoverCurrentProject(t *testing.T) {
	base := t.TempDir()
	projDir := setupProject(t, base, "my-app", ".agent-mail", []string{"collab", "auth"})

	proj, err := DiscoverProject(projDir)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Slug != "my-app" {
		t.Errorf("slug = %q, want my-app", proj.Slug)
	}
	if len(proj.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(proj.Sessions))
	}
}

func TestDiscoverSessions(t *testing.T) {
	base := t.TempDir()
	projDir := setupProject(t, base, "my-app", ".agent-mail", []string{"collab", "auth", "api"})

	proj, _ := DiscoverProject(projDir)
	sessions := proj.Sessions
	if len(sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(sessions))
	}
	names := make(map[string]bool)
	for _, s := range sessions {
		names[s.Name] = true
	}
	for _, want := range []string{"collab", "auth", "api"} {
		if !names[want] {
			t.Errorf("missing session %q", want)
		}
	}
}

func TestDiscoverAgentsInSession(t *testing.T) {
	base := t.TempDir()
	projDir := setupProject(t, base, "my-app", ".agent-mail", []string{"collab"})

	proj, _ := DiscoverProject(projDir)
	agents := proj.Sessions[0].Agents
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
}

func TestScanProjects(t *testing.T) {
	base := t.TempDir()
	setupProject(t, base, "app-a", ".agent-mail", []string{"collab"})
	setupProject(t, base, "app-b", ".agent-mail", []string{"collab"})
	setupProject(t, base, "no-amq", ".agent-mail", nil) // has .amqrc but no sessions

	projects, err := ScanProjects([]string{base}, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Should find app-a and app-b (no-amq has no sessions but still valid)
	if len(projects) < 2 {
		t.Errorf("found %d projects, want >= 2", len(projects))
	}
}

func TestDiscoverProject_NoAmqrc(t *testing.T) {
	dir := t.TempDir()
	_, err := DiscoverProject(dir)
	if err == nil {
		t.Fatal("expected error for directory without .amqrc")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd .claude/worktrees/federation && go test ./internal/discover/ -v`
Expected: compilation error

- [ ] **Step 3: Implement project discovery**

```go
// internal/discover/discover.go
package discover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are directories never scanned during discovery.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	".venv": true, "dist": true, "build": true,
	".agent-mail": true, "__pycache__": true, ".cache": true,
}

// Project represents a discovered AMQ-enabled project.
type Project struct {
	Slug      string    // directory basename (or .amqrc "project" field)
	ProjectID string    // optional stable ID from .amqrc
	Dir       string    // absolute path to project directory
	BaseRoot  string    // absolute path to AMQ base root
	AmqrcPath string    // absolute path to .amqrc
	Sessions  []Session // discovered sessions
}

// Session represents a discovered session within a project.
type Session struct {
	Name   string   // session directory name
	Root   string   // absolute path to session root
	Agents []string // discovered agent handles
}

// amqrc represents the .amqrc configuration file.
type amqrc struct {
	Root      string `json:"root"`
	Project   string `json:"project,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// DiscoverProject discovers the AMQ project at or above the given directory.
func DiscoverProject(startDir string) (Project, error) {
	absDir, err := filepath.Abs(startDir)
	if err != nil {
		return Project{}, err
	}

	// Walk up to find .amqrc
	dir := absDir
	for {
		rcPath := filepath.Join(dir, ".amqrc")
		if _, err := os.Stat(rcPath); err == nil {
			return loadProject(dir, rcPath)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return Project{}, fmt.Errorf("no .amqrc found at or above %s", startDir)
}

func loadProject(projDir, rcPath string) (Project, error) {
	data, err := os.ReadFile(rcPath)
	if err != nil {
		return Project{}, fmt.Errorf("read .amqrc: %w", err)
	}
	var rc amqrc
	if err := json.Unmarshal(data, &rc); err != nil {
		return Project{}, fmt.Errorf("parse .amqrc: %w", err)
	}

	baseRoot := rc.Root
	if !filepath.IsAbs(baseRoot) {
		baseRoot = filepath.Join(projDir, baseRoot)
	}

	slug := rc.Project
	if slug == "" {
		slug = filepath.Base(projDir)
	}

	proj := Project{
		Slug:      slug,
		ProjectID: rc.ProjectID,
		Dir:       projDir,
		BaseRoot:  baseRoot,
		AmqrcPath: rcPath,
	}

	// Enumerate sessions
	proj.Sessions, _ = discoverSessions(baseRoot)
	return proj, nil
}

func discoverSessions(baseRoot string) ([]Session, error) {
	entries, err := os.ReadDir(baseRoot)
	if err != nil {
		return nil, err
	}

	var sessions []Session
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sessDir := filepath.Join(baseRoot, e.Name())
		agentsDir := filepath.Join(sessDir, "agents")
		if _, err := os.Stat(agentsDir); err != nil {
			continue // not a session
		}
		agents, _ := discoverAgents(agentsDir)
		sessions = append(sessions, Session{
			Name:   e.Name(),
			Root:   sessDir,
			Agents: agents,
		})
	}
	return sessions, nil
}

func discoverAgents(agentsDir string) ([]string, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, err
	}
	var agents []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Verify inbox exists
		inbox := filepath.Join(agentsDir, e.Name(), "inbox")
		if _, err := os.Stat(inbox); err == nil {
			agents = append(agents, e.Name())
		}
	}
	return agents, nil
}

// ScanProjects scans the given root directories for AMQ projects.
// maxDepth limits how deep to search from each root.
func ScanProjects(roots []string, maxDepth int) ([]Project, error) {
	var projects []Project
	seen := make(map[string]bool)

	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		scanDir(absRoot, 0, maxDepth, seen, &projects)
	}
	return projects, nil
}

func scanDir(dir string, depth, maxDepth int, seen map[string]bool, projects *[]Project) {
	if depth > maxDepth {
		return
	}
	if seen[dir] {
		return
	}

	rcPath := filepath.Join(dir, ".amqrc")
	if _, err := os.Stat(rcPath); err == nil {
		seen[dir] = true
		proj, err := loadProject(dir, rcPath)
		if err == nil {
			*projects = append(*projects, proj)
		}
		return // don't recurse into AMQ projects
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || skipDirs[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		scanDir(filepath.Join(dir, e.Name()), depth+1, maxDepth, seen, projects)
	}
}
```

- [ ] **Step 4: Run tests and verify they pass**

Run: `cd .claude/worktrees/federation && go test ./internal/discover/ -v -run TestDiscover`
Expected: ALL PASS

- [ ] **Step 5: Implement discovery cache**

```go
// internal/discover/cache.go
package discover

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const cacheVersion = 1

// CacheEntry represents a cached project discovery result.
type CacheEntry struct {
	Slug       string    `json:"slug"`
	ProjectID  string    `json:"project_id,omitempty"`
	Dir        string    `json:"dir"`
	AmqrcPath  string    `json:"amqrc_path"`
	BaseRoot   string    `json:"base_root"`
	AmqrcHash  string    `json:"amqrc_hash"`
	VerifiedAt time.Time `json:"verified_at"`
}

// Cache holds discovery cache state.
type Cache struct {
	Version int          `json:"version"`
	Entries []CacheEntry `json:"entries"`
}

// DefaultCachePath returns the default cache file path.
func DefaultCachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "amq", "discovery-v1.json")
}

// LoadCache reads the discovery cache from disk.
func LoadCache(path string) (Cache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Cache{Version: cacheVersion}, nil // empty cache on missing file
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return Cache{Version: cacheVersion}, nil // reset on corrupt cache
	}
	if c.Version != cacheVersion {
		return Cache{Version: cacheVersion}, nil
	}
	return c, nil
}

// SaveCache writes the discovery cache to disk.
func SaveCache(path string, c Cache) error {
	c.Version = cacheVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Validate checks if a cache entry is still valid by verifying the .amqrc file.
func (e CacheEntry) Validate() bool {
	data, err := os.ReadFile(e.AmqrcPath)
	if err != nil {
		return false
	}
	return hashBytes(data) == e.AmqrcHash
}

// FindBySlug returns the first cache entry matching the given slug.
func (c Cache) FindBySlug(slug string) (CacheEntry, bool) {
	for _, e := range c.Entries {
		if e.Slug == slug {
			return e, true
		}
	}
	return CacheEntry{}, false
}

// Update adds or refreshes a cache entry for a discovered project.
func (c *Cache) Update(proj Project) {
	data, _ := os.ReadFile(proj.AmqrcPath)
	entry := CacheEntry{
		Slug:       proj.Slug,
		ProjectID:  proj.ProjectID,
		Dir:        proj.Dir,
		AmqrcPath:  proj.AmqrcPath,
		BaseRoot:   proj.BaseRoot,
		AmqrcHash:  hashBytes(data),
		VerifiedAt: time.Now(),
	}

	for i, e := range c.Entries {
		if e.Dir == proj.Dir {
			c.Entries[i] = entry
			return
		}
	}
	c.Entries = append(c.Entries, entry)
}

// Prune removes entries that no longer validate.
func (c *Cache) Prune() {
	valid := make([]CacheEntry, 0, len(c.Entries))
	for _, e := range c.Entries {
		if e.Validate() {
			valid = append(valid, e)
		}
	}
	c.Entries = valid
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}
```

- [ ] **Step 6: Write cache tests**

```go
// internal/discover/cache_test.go
package discover

import (
	"path/filepath"
	"testing"
)

func TestCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c := Cache{Entries: []CacheEntry{{Slug: "my-app", Dir: "/tmp/my-app"}}}
	if err := SaveCache(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Slug != "my-app" {
		t.Fatalf("unexpected: %+v", loaded)
	}
}

func TestCache_FindBySlug(t *testing.T) {
	c := Cache{Entries: []CacheEntry{
		{Slug: "app-a", Dir: "/a"},
		{Slug: "app-b", Dir: "/b"},
	}}
	entry, ok := c.FindBySlug("app-b")
	if !ok || entry.Dir != "/b" {
		t.Fatalf("unexpected: %+v", entry)
	}
	_, ok = c.FindBySlug("nope")
	if ok {
		t.Fatal("should not find nonexistent slug")
	}
}

func TestCache_MissingFile(t *testing.T) {
	c, err := LoadCache("/nonexistent/cache.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 0 {
		t.Fatal("should be empty cache")
	}
}
```

- [ ] **Step 7: Run all discovery tests**

Run: `cd .claude/worktrees/federation && go test ./internal/discover/ -v`
Expected: ALL PASS

- [ ] **Step 8: Commit**

```bash
cd .claude/worktrees/federation
git add internal/discover/
git commit -m "feat: add project discovery with filesystem scanning and cache

Convention-based: scans for .amqrc files.
Cache at ~/.cache/amq/discovery-v1.json is never authoritative.
Enumerates sessions and agents by directory structure."
```

---

## Task 3: Session and Agent Metadata

Read/write `session.json` and `agent.json` for presence, topic, claims, and channel membership.

**Files:**
- Create: `internal/metadata/session.go`
- Create: `internal/metadata/agent.go`
- Create: `internal/metadata/metadata_test.go`
- Modify: `internal/fsq/layout.go` — add `SessionJSON()`, `AgentJSON()` helpers

- [ ] **Step 1: Write failing tests**

```go
// internal/metadata/metadata_test.go
package metadata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionMetadata_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	s := SessionMeta{
		Schema:  1,
		Session: "auth",
		Topic:   "Auth rewrite",
		Branch:  "feat/auth-v2",
		Claims:  []string{"internal/auth/**"},
		Updated: time.Now().UTC().Truncate(time.Second),
	}
	if err := WriteSessionMeta(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Session != "auth" || loaded.Topic != "Auth rewrite" || len(loaded.Claims) != 1 {
		t.Fatalf("unexpected: %+v", loaded)
	}
}

func TestAgentMeta_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	a := AgentMeta{
		Schema:   1,
		Agent:    "claude",
		LastSeen: time.Now().UTC().Truncate(time.Second),
		Channels: []string{"events", "triage"},
	}
	if err := WriteAgentMeta(path, a); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadAgentMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent != "claude" || len(loaded.Channels) != 2 {
		t.Fatalf("unexpected: %+v", loaded)
	}
}

func TestAgentMeta_TouchLastSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	a := AgentMeta{Schema: 1, Agent: "claude", LastSeen: time.Now().Add(-time.Hour).UTC()}
	WriteAgentMeta(path, a)

	if err := TouchLastSeen(path); err != nil {
		t.Fatal(err)
	}
	loaded, _ := ReadAgentMeta(path)
	if time.Since(loaded.LastSeen) > 5*time.Second {
		t.Fatalf("last_seen not updated: %v", loaded.LastSeen)
	}
}

func TestAgentMeta_IsActive(t *testing.T) {
	recent := AgentMeta{LastSeen: time.Now().UTC()}
	if !recent.IsActive(10 * time.Minute) {
		t.Fatal("recent agent should be active")
	}
	stale := AgentMeta{LastSeen: time.Now().Add(-time.Hour).UTC()}
	if stale.IsActive(10 * time.Minute) {
		t.Fatal("stale agent should not be active")
	}
}

func TestReadSessionMeta_Missing(t *testing.T) {
	_, err := ReadSessionMeta("/nonexistent/session.json")
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd .claude/worktrees/federation && go test ./internal/metadata/ -v`
Expected: compilation error

- [ ] **Step 3: Implement metadata package**

```go
// internal/metadata/session.go
package metadata

import (
	"encoding/json"
	"os"
	"time"
)

// SessionMeta represents the session.json advisory metadata file.
type SessionMeta struct {
	Schema  int       `json:"schema"`
	Session string    `json:"session"`
	Topic   string    `json:"topic,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Claims  []string  `json:"claims,omitempty"`
	Updated time.Time `json:"updated"`
}

func WriteSessionMeta(path string, s SessionMeta) error {
	s.Schema = 1
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func ReadSessionMeta(path string) (SessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionMeta{}, err
	}
	var s SessionMeta
	if err := json.Unmarshal(data, &s); err != nil {
		return SessionMeta{}, err
	}
	return s, nil
}
```

```go
// internal/metadata/agent.go
package metadata

import (
	"encoding/json"
	"os"
	"time"
)

// AgentMeta represents the agent.json advisory metadata file.
type AgentMeta struct {
	Schema   int       `json:"schema"`
	Agent    string    `json:"agent"`
	LastSeen time.Time `json:"last_seen"`
	Channels []string  `json:"channels,omitempty"`
}

func WriteAgentMeta(path string, a AgentMeta) error {
	a.Schema = 1
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func ReadAgentMeta(path string) (AgentMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentMeta{}, err
	}
	var a AgentMeta
	if err := json.Unmarshal(data, &a); err != nil {
		return AgentMeta{}, err
	}
	return a, nil
}

// TouchLastSeen updates the last_seen field to now.
func TouchLastSeen(path string) error {
	a, err := ReadAgentMeta(path)
	if err != nil {
		return err
	}
	a.LastSeen = time.Now().UTC()
	return WriteAgentMeta(path, a)
}

// IsActive returns true if the agent was seen within the given TTL.
func (a AgentMeta) IsActive(ttl time.Duration) bool {
	return time.Since(a.LastSeen) < ttl
}
```

- [ ] **Step 4: Add path helpers to fsq/layout.go**

Add to `internal/fsq/layout.go`:

```go
// SessionJSON returns the path to the session metadata file.
func SessionJSON(sessionRoot string) string {
	return filepath.Join(sessionRoot, "session.json")
}

// AgentJSON returns the path to the agent metadata file.
func AgentJSON(root, agent string) string {
	return filepath.Join(root, "agents", agent, "agent.json")
}
```

- [ ] **Step 5: Run all tests**

Run: `cd .claude/worktrees/federation && go test ./internal/metadata/ ./internal/fsq/ -v`
Expected: ALL PASS

- [ ] **Step 6: Commit**

```bash
cd .claude/worktrees/federation
git add internal/metadata/ internal/fsq/layout.go
git commit -m "feat: add session and agent metadata files

session.json: topic, branch, file claims, update time.
agent.json: last_seen, channel memberships.
Advisory metadata, not required for delivery."
```

---

## Task 4: Message Format v2 (Origin + Delivery)

Add `Origin` and `Delivery` structs to the message header. Bump schema to 2, maintain backward compatibility with schema 1.

**Files:**
- Modify: `internal/format/message.go`
- Modify: `internal/format/message_test.go`

- [ ] **Step 1: Write failing tests for schema 2**

Add to `internal/format/message_test.go`:

```go
func TestMessageRoundTrip_Schema2(t *testing.T) {
	msg := Message{
		Header: Header{
			From:    "claude",
			To:      []string{"codex"},
			Thread:  "p2p/claude__codex",
			Subject: "Cross-session test",
			Origin: &Origin{
				Project:   "my-app",
				ProjectID: "abc123",
				Session:   "auth",
				Agent:     "claude",
				ReplyTo:   "claude@my-app:auth",
			},
			Delivery: &Delivery{
				RequestedTo: []string{"codex@api"},
				ResolvedTo:  []string{"codex@my-app:api"},
				Scope:       "cross-session",
			},
		},
		Body: "Hello from auth session",
	}

	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Schema != CurrentSchema {
		t.Errorf("schema = %d, want %d", parsed.Header.Schema, CurrentSchema)
	}
	if parsed.Header.Origin == nil {
		t.Fatal("origin should not be nil")
	}
	if parsed.Header.Origin.Project != "my-app" {
		t.Errorf("origin.project = %q", parsed.Header.Origin.Project)
	}
	if parsed.Header.Origin.ReplyTo != "claude@my-app:auth" {
		t.Errorf("origin.reply_to = %q", parsed.Header.Origin.ReplyTo)
	}
	if parsed.Header.Delivery == nil {
		t.Fatal("delivery should not be nil")
	}
	if parsed.Header.Delivery.Scope != "cross-session" {
		t.Errorf("delivery.scope = %q", parsed.Header.Delivery.Scope)
	}
}

func TestParseMessage_Schema1_BackwardCompat(t *testing.T) {
	// Schema 1 messages should still parse fine with nil origin/delivery
	raw := "---json\n{\"schema\":1,\"id\":\"test\",\"from\":\"codex\",\"to\":[\"claude\"],\"thread\":\"t\",\"created\":\"2026-01-01T00:00:00Z\"}\n---\nHello"
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Origin != nil {
		t.Error("schema 1 should have nil origin")
	}
	if msg.Header.Delivery != nil {
		t.Error("schema 1 should have nil delivery")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd .claude/worktrees/federation && go test ./internal/format/ -v -run "Schema2|BackwardCompat"`
Expected: FAIL (Origin/Delivery types don't exist)

- [ ] **Step 3: Add Origin and Delivery to Header**

Add to `internal/format/message.go` after the `Header` struct:

```go
// Origin identifies the source of a cross-session or cross-project message.
type Origin struct {
	Project   string `json:"project,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Session   string `json:"session,omitempty"`
	Agent     string `json:"agent,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
}

// Delivery records how the message was routed.
type Delivery struct {
	RequestedTo []string `json:"requested_to,omitempty"`
	ResolvedTo  []string `json:"resolved_to,omitempty"`
	Scope       string   `json:"scope,omitempty"` // "local", "cross-session", "cross-project"
	Channel     string   `json:"channel,omitempty"`
	FanoutIndex int      `json:"fanout_index,omitempty"`
	FanoutTotal int      `json:"fanout_total,omitempty"`
}
```

Add fields to `Header`:

```go
	// Federation fields (schema 2, optional for backward compatibility)
	Origin   *Origin   `json:"origin,omitempty"`
	Delivery *Delivery `json:"delivery,omitempty"`
```

Bump `CurrentSchema` to `2`.

- [ ] **Step 4: Run tests and verify they pass**

Run: `cd .claude/worktrees/federation && go test ./internal/format/ -v`
Expected: ALL PASS (including existing tests — Go JSON ignores unknown fields)

- [ ] **Step 5: Commit**

```bash
cd .claude/worktrees/federation
git add internal/format/
git commit -m "feat: add Origin and Delivery to message header (schema v2)

Origin: project, session, agent, reply_to for cross-boundary identity.
Delivery: requested_to, resolved_to, scope, channel info.
Backward compatible: schema 1 messages parse with nil origin/delivery."
```

---

## Task 5: Address Resolver

Resolve parsed `Endpoint` addresses to concrete filesystem paths for delivery. Integrates discovery for cross-project resolution.

**Files:**
- Create: `internal/resolve/resolver.go`
- Create: `internal/resolve/resolver_test.go`

- [ ] **Step 1: Write failing tests for resolver**

```go
// internal/resolve/resolver_test.go
package resolve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/discover"
)

func setupTestProject(t *testing.T, base, name string, sessions map[string][]string) string {
	t.Helper()
	projDir := filepath.Join(base, name)
	os.MkdirAll(projDir, 0o700)
	amqrc := map[string]string{"root": ".agent-mail", "project": name}
	data, _ := json.Marshal(amqrc)
	os.WriteFile(filepath.Join(projDir, ".amqrc"), data, 0o600)
	for sess, agents := range sessions {
		for _, agent := range agents {
			dir := filepath.Join(projDir, ".agent-mail", sess, "agents", agent, "inbox", "new")
			os.MkdirAll(dir, 0o700)
			os.MkdirAll(filepath.Join(projDir, ".agent-mail", sess, "agents", agent, "inbox", "tmp"), 0o700)
			os.MkdirAll(filepath.Join(projDir, ".agent-mail", sess, "agents", agent, "inbox", "cur"), 0o700)
		}
	}
	return projDir
}

func TestResolveLocal(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
	})
	sessionRoot := filepath.Join(proj, ".agent-mail", "collab")

	r := NewResolver(sessionRoot, filepath.Join(proj, ".agent-mail"), proj)
	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Agent != "codex" {
		t.Fatalf("unexpected: %+v", targets)
	}
}

func TestResolveCrossSession(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
		"auth":   {"claude", "codex"},
	})
	sessionRoot := filepath.Join(proj, ".agent-mail", "collab")

	r := NewResolver(sessionRoot, filepath.Join(proj, ".agent-mail"), proj)
	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].SessionRoot != filepath.Join(proj, ".agent-mail", "auth") {
		t.Errorf("wrong session root: %s", targets[0].SessionRoot)
	}
}

func TestResolveCrossProject(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
	})
	infraProj := setupTestProject(t, base, "infra-lib", map[string][]string{
		"collab": {"claude", "codex"},
	})
	_ = infraProj

	appRoot := filepath.Join(base, "my-app", ".agent-mail", "collab")
	r := NewResolver(appRoot, filepath.Join(base, "my-app", ".agent-mail"), filepath.Join(base, "my-app"))
	r.DiscoveryRoots = []string{base}

	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "claude", Project: "infra-lib", Session: "collab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
}

func TestResolveChannel_All(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
		"auth":   {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	targets, err := r.Resolve(Endpoint{Kind: KindChannel, Channel: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// #all should fan out to all agents in all sessions = 4
	if len(targets) != 4 {
		t.Fatalf("want 4 targets for #all, got %d", len(targets))
	}
}

func TestResolveChannel_Session(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
		"auth":   {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	targets, err := r.Resolve(Endpoint{Kind: KindChannel, Channel: "session", Session: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("want 2 targets for #session/auth, got %d", len(targets))
	}
}

func TestResolve_NotFound(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd .claude/worktrees/federation && go test ./internal/resolve/ -v -run Resolve`
Expected: compilation error

- [ ] **Step 3: Implement resolver**

```go
// internal/resolve/resolver.go
package resolve

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/discover"
)

// Target represents a resolved delivery target.
type Target struct {
	Agent       string
	Session     string
	SessionRoot string // absolute path to the session root for delivery
	Project     string
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

	// Cross-project
	if ep.IsCrossProject() {
		return r.resolveCrossProjectAgent(ep)
	}

	// Cross-session (same project)
	return r.resolveCrossSessionAgent(ep)
}

func (r *Resolver) resolveLocalAgent(agent string) ([]Target, error) {
	inbox := filepath.Join(r.SessionRoot, "agents", agent, "inbox")
	if _, err := os.Stat(inbox); err != nil {
		return nil, fmt.Errorf("agent %q not found in current session", agent)
	}
	return []Target{{
		Agent:       agent,
		Session:     filepath.Base(r.SessionRoot),
		SessionRoot: r.SessionRoot,
	}}, nil
}

func (r *Resolver) resolveCrossSessionAgent(ep Endpoint) ([]Target, error) {
	sessRoot := filepath.Join(r.BaseRoot, ep.Session)
	inbox := filepath.Join(sessRoot, "agents", ep.Agent, "inbox")
	if _, err := os.Stat(inbox); err != nil {
		return nil, fmt.Errorf("agent %q not found in session %q", ep.Agent, ep.Session)
	}
	return []Target{{
		Agent:       ep.Agent,
		Session:     ep.Session,
		SessionRoot: sessRoot,
	}}, nil
}

func (r *Resolver) resolveCrossProjectAgent(ep Endpoint) ([]Target, error) {
	proj, err := r.findProject(ep.Project)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", ep.Project, err)
	}

	if ep.Session != "" {
		// Explicit session
		sessRoot := filepath.Join(proj.BaseRoot, ep.Session)
		inbox := filepath.Join(sessRoot, "agents", ep.Agent, "inbox")
		if _, err := os.Stat(inbox); err != nil {
			return nil, fmt.Errorf("agent %q not found in %s:%s", ep.Agent, ep.Project, ep.Session)
		}
		return []Target{{
			Agent:       ep.Agent,
			Session:     ep.Session,
			SessionRoot: sessRoot,
			Project:     ep.Project,
		}}, nil
	}

	// Search all sessions for the agent (must be unique)
	var matches []Target
	for _, sess := range proj.Sessions {
		for _, agent := range sess.Agents {
			if agent == ep.Agent {
				matches = append(matches, Target{
					Agent:       agent,
					Session:     sess.Name,
					SessionRoot: sess.Root,
					Project:     ep.Project,
				})
			}
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("agent %q not found in project %q", ep.Agent, ep.Project)
	case 1:
		return matches, nil
	default:
		sessions := make([]string, len(matches))
		for i, m := range matches {
			sessions[i] = m.Session
		}
		return nil, fmt.Errorf("agent %q is ambiguous in project %q (found in sessions: %v); use %s@%s:<session>",
			ep.Agent, ep.Project, sessions, ep.Agent, ep.Project)
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
		baseRoot = proj.BaseRoot
		projectSlug = ep.Project
	}

	proj, err := discover.DiscoverProject(filepath.Dir(baseRoot))
	if err != nil {
		// Fallback: scan sessions directly
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

	for _, sess := range sessions {
		// #session/X: only agents in that session
		if ep.Channel == "session" && ep.Session != "" {
			if sess.Name != ep.Session {
				continue
			}
		}

		for _, agent := range sess.Agents {
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

func (r *Resolver) findProject(slug string) (discover.Project, error) {
	// Try discovery cache first
	cachePath := discover.DefaultCachePath()
	cache, _ := discover.LoadCache(cachePath)
	if entry, ok := cache.FindBySlug(slug); ok && entry.Validate() {
		proj, err := discover.DiscoverProject(entry.Dir)
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

	// Find by slug
	for _, p := range projects {
		if p.Slug == slug {
			return p, nil
		}
	}

	return discover.Project{}, fmt.Errorf("not found")
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
```

- [ ] **Step 4: Run tests and verify they pass**

Run: `cd .claude/worktrees/federation && go test ./internal/resolve/ -v`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
cd .claude/worktrees/federation
git add internal/resolve/
git commit -m "feat: add address resolver for cross-session and cross-project routing

Resolves qualified addresses to concrete Maildir paths.
Integrates filesystem discovery for cross-project lookups.
Channels resolve to fan-out over agent inboxes.
Hard errors on ambiguity (no silent fallback)."
```

---

## Task 6: Integrate Resolver into Send and Reply

Wire the address parser and resolver into `amq send` and `amq reply`. This is the core behavioral change.

**Files:**
- Modify: `internal/cli/send.go`
- Modify: `internal/cli/reply.go`
- Modify: `internal/cli/common.go`
- Modify: `internal/cli/env.go`

- [ ] **Step 1: Add new env var constants to common.go**

Add to `internal/cli/common.go`:

```go
const (
	envProject  = "AM_PROJECT"
	envSession  = "AM_SESSION"
	envBaseRoot = "AM_BASE_ROOT"
)
```

- [ ] **Step 2: Extend .amqrc struct in env.go**

Update the `amqrc` struct in `internal/cli/env.go`:

```go
type amqrc struct {
	Root      string `json:"root"`
	Project   string `json:"project,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}
```

- [ ] **Step 3: Modify send.go to support qualified addresses**

Key changes to `runSend()`:
- Replace `splitRecipients()` with parsing each recipient through `resolve.ParseAddress()`
- For local-only sends (all bare handles), use existing path
- For qualified sends, use resolver to get target paths
- Populate `Origin` and `Delivery` on the message
- Bypass `AM_ROOT` guard for qualified addresses (the resolver handles path resolution)
- For channel sends, fan-out to all resolved targets

This is a significant refactor of `runSend()`. The agent implementing this should read the full current `send.go`, understand the flow, then modify it to integrate the resolver while keeping backward compatibility for bare handles.

- [ ] **Step 4: Modify reply.go to use origin.reply_to**

Key changes to `runReply()`:
- When the original message has `Origin.ReplyTo`, parse it as a qualified address
- Route the reply through the resolver
- Fall back to legacy `from` field behavior when no origin is present

- [ ] **Step 5: Write integration test**

Create `internal/cli/federation_test.go` with an end-to-end test:
- Set up two sessions with agents
- Send a cross-session message via `runSend()`
- Verify it arrives in the target session's inbox
- Reply to it and verify the reply routes back

- [ ] **Step 6: Run full test suite**

Run: `cd .claude/worktrees/federation && go test ./... -v`
Expected: ALL PASS

- [ ] **Step 7: Commit**

```bash
cd .claude/worktrees/federation
git add internal/cli/
git commit -m "feat: integrate federation resolver into send and reply

amq send --to codex@auth routes cross-session.
amq send --to claude@infra-lib:auth routes cross-project.
amq reply uses origin.reply_to for cross-boundary replies.
Backward compatible: bare handles still route locally."
```

---

## Task 7: New CLI Commands

Add `discover`, `who`, `resolve`, `channel`, and `announce` commands.

**Files:**
- Create: `internal/cli/discover.go`
- Create: `internal/cli/who.go`
- Create: `internal/cli/resolve_cmd.go`
- Create: `internal/cli/channel.go`
- Create: `internal/cli/announce.go`
- Modify: `internal/cli/cli.go` — register new commands

- [ ] **Step 1: Implement `amq discover`**

Scans for projects, shows discovered projects/sessions/agents, updates cache.

- [ ] **Step 2: Implement `amq who`**

Shows presence across sessions: session name, agents, status, topic, branch, claimed paths.

- [ ] **Step 3: Implement `amq resolve`**

Debug command: parses and resolves an address, shows the resolution path.

- [ ] **Step 4: Implement `amq channel join/leave/list`**

Manages agent.json channel memberships. `join` adds a channel, `leave` removes it, `list` shows current memberships.

- [ ] **Step 5: Implement `amq announce`**

Sugar for `amq send --to '#channel'`. Resolves channel, fans out to inboxes.

- [ ] **Step 6: Register all commands in cli.go**

Add cases to the main switch in `Run()`.

- [ ] **Step 7: Run full test suite**

Run: `cd .claude/worktrees/federation && go test ./... -v`
Expected: ALL PASS

- [ ] **Step 8: Commit**

```bash
cd .claude/worktrees/federation
git add internal/cli/
git commit -m "feat: add discover, who, resolve, channel, announce CLI commands

amq discover — scan and list AMQ-enabled projects
amq who — presence view across sessions
amq resolve — debug address resolution
amq channel join/leave/list — manage channel memberships
amq announce — fan-out to channel subscribers"
```

---

## Task 8: Coop Exec Metadata and Env Vars

Update `coop exec` to write metadata files and set new environment variables.

**Files:**
- Modify: `internal/cli/coop_exec_unix.go`
- Modify: `internal/cli/coop.go`

- [ ] **Step 1: Add flags to coop exec**

Add `--topic`, `--claim`, `--channel` flags.

- [ ] **Step 2: Write session.json and agent.json on exec**

After creating session dirs, write metadata files with topic, branch (auto-detect from git), claims, and channel memberships.

- [ ] **Step 3: Set new env vars**

Add `AM_PROJECT`, `AM_SESSION`, `AM_BASE_ROOT` to the exec environment.

- [ ] **Step 4: Test**

Run: `cd .claude/worktrees/federation && go test ./internal/cli/ -v -run Coop`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd .claude/worktrees/federation
git add internal/cli/
git commit -m "feat: coop exec writes metadata and sets federation env vars

Writes session.json (topic, branch, claims) and agent.json (channels).
Sets AM_PROJECT, AM_SESSION, AM_BASE_ROOT alongside AM_ROOT and AM_ME."
```

---

## Task 9: Documentation and Skill Updates

Update CLAUDE.md, COOP.md, and the AMQ skill with federation documentation.

**Files:**
- Modify: `CLAUDE.md`
- Modify: `COOP.md`
- Modify: `.claude/skills/amq-cli/SKILL.md`
- Modify: `.claude/skills/amq-cli/references/coop-mode.md`

- [ ] **Step 1: Update CLAUDE.md with federation sections**

Add addressing scheme, new commands, new env vars, cross-session/project examples.

- [ ] **Step 2: Update COOP.md with multi-stream coordination**

Add "Multiple Streams on Same Project" section with examples.

- [ ] **Step 3: Update skill files**

- [ ] **Step 4: Sync skills**

Run: `make sync-skills`

- [ ] **Step 5: Commit**

```bash
cd .claude/worktrees/federation
git add CLAUDE.md COOP.md .claude/skills/ .codex/skills/ skills/
git commit -m "docs: add federation documentation

Qualified addressing, cross-session/project routing,
channels, discovery, new CLI commands."
```

---

## Execution Notes

**Task dependencies:**
- Tasks 1-4 are independent and can be parallelized
- Task 5 depends on Tasks 1 + 2
- Task 6 depends on Tasks 4 + 5
- Task 7 depends on Tasks 2 + 3 + 5
- Task 8 depends on Task 3
- Task 9 depends on all previous tasks

**Parallel execution plan:**
- Wave 1: Tasks 1, 2, 3, 4 (all independent)
- Wave 2: Task 5 (needs 1+2)
- Wave 3: Tasks 6, 7, 8 (need 5, can partly parallelize)
- Wave 4: Task 9 (docs)
