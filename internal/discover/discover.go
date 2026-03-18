// internal/discover/discover.go
package discover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// nameRe matches valid session and agent names: lowercase letters, digits, underscore, hyphen.
var nameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

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
		// Skip entries with invalid names (e.g., path traversal, uppercase, spaces).
		if !nameRe.MatchString(e.Name()) {
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
		// Skip entries with invalid names (e.g., path traversal, uppercase, spaces).
		if !nameRe.MatchString(e.Name()) {
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
