package cli

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/discover"
)

type discoverResult struct {
	Slug      string            `json:"slug"`
	ProjectID string            `json:"project_id,omitempty"`
	Dir       string            `json:"dir"`
	BaseRoot  string            `json:"base_root"`
	Sessions  []discoverSession `json:"sessions"`
}

type discoverSession struct {
	Name   string   `json:"name"`
	Agents []string `json:"agents"`
}

func runDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Emit JSON output")
	refreshFlag := fs.Bool("refresh", false, "Force full rescan (ignore cache)")

	usage := usageWithFlags(fs, "amq discover [--refresh] [--json]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// Determine discovery roots: parent of current project by default
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Try to find the current project dir (walk up for .amqrc)
	projectDir := cwd
	proj, projErr := discover.DiscoverProject(cwd)
	if projErr == nil {
		projectDir = proj.Dir
	}

	roots := []string{filepath.Dir(projectDir)}

	cachePath := discover.DefaultCachePath()

	if !*refreshFlag {
		// Try cache first
		cache, _ := discover.LoadCache(cachePath)
		if len(cache.Entries) > 0 {
			// Validate cached entries and display them
			var results []discoverResult
			for _, entry := range cache.Entries {
				if !entry.Validate() {
					continue
				}
				p, err := discover.DiscoverProject(entry.Dir)
				if err != nil {
					continue
				}
				results = append(results, projectToResult(p))
			}
			if len(results) > 0 {
				return printDiscoverResults(results, *jsonFlag)
			}
			// Cache was all stale, fall through to scan
		}
	}

	// Full scan
	projects, err := discover.ScanProjects(roots, 2)
	if err != nil {
		return err
	}

	// Update cache
	cache, _ := discover.LoadCache(cachePath)
	for _, p := range projects {
		cache.Update(p)
	}
	_ = discover.SaveCache(cachePath, cache)

	results := make([]discoverResult, 0, len(projects))
	for _, p := range projects {
		results = append(results, projectToResult(p))
	}

	return printDiscoverResults(results, *jsonFlag)
}

func projectToResult(p discover.Project) discoverResult {
	sessions := make([]discoverSession, 0, len(p.Sessions))
	for _, s := range p.Sessions {
		sessions = append(sessions, discoverSession{
			Name:   s.Name,
			Agents: s.Agents,
		})
	}
	return discoverResult{
		Slug:      p.Slug,
		ProjectID: p.ProjectID,
		Dir:       p.Dir,
		BaseRoot:  p.BaseRoot,
		Sessions:  sessions,
	}
}

func printDiscoverResults(results []discoverResult, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(os.Stdout, results)
	}

	if len(results) == 0 {
		return writeStdoutLine("No AMQ projects found.")
	}

	for i, r := range results {
		if i > 0 {
			if err := writeStdoutLine(""); err != nil {
				return err
			}
		}
		if err := writeStdout("Project: %s\n", r.Slug); err != nil {
			return err
		}
		if r.ProjectID != "" {
			if err := writeStdout("  ID:   %s\n", r.ProjectID); err != nil {
				return err
			}
		}
		if err := writeStdout("  Path: %s\n", r.Dir); err != nil {
			return err
		}
		if len(r.Sessions) == 0 {
			if err := writeStdout("  Sessions: (none)\n"); err != nil {
				return err
			}
		} else {
			for _, s := range r.Sessions {
				agentList := "(no agents)"
				if len(s.Agents) > 0 {
					agentList = ""
					for j, a := range s.Agents {
						if j > 0 {
							agentList += ", "
						}
						agentList += a
					}
				}
				if err := writeStdout("  Session: %-12s  Agents: %s\n", s.Name, agentList); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
