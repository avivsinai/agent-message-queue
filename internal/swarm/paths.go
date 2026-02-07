package swarm

import (
	"fmt"
	"os"
	"path/filepath"
)

// Default Claude Code teams and tasks directories.
const (
	claudeConfigDir = ".claude"
	teamsSubdir     = "teams"
	tasksSubdir     = "tasks"
	teamConfigFile  = "config.json"
)

// ClaudeTeamsDir returns the path to ~/.claude/teams/.
func ClaudeTeamsDir() string {
	return filepath.Join(claudeHome(), teamsSubdir)
}

// ClaudeTasksDir returns the path to ~/.claude/tasks/.
func ClaudeTasksDir() string {
	return filepath.Join(claudeHome(), tasksSubdir)
}

// TeamConfigPath returns the path to a team's config.json.
func TeamConfigPath(team string) string {
	return filepath.Join(ClaudeTeamsDir(), team, teamConfigFile)
}

// TeamTasksDir returns the path to a team's tasks directory.
func TeamTasksDir(team string) string {
	return filepath.Join(ClaudeTasksDir(), team)
}

func claudeHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Avoid silently writing to CWD — surface the error so callers
		// notice they're getting a relative path.
		_, _ = fmt.Fprintf(os.Stderr, "warning: unable to determine home directory: %v; using ./%s\n", err, claudeConfigDir)
		return filepath.Join(".", claudeConfigDir)
	}
	return filepath.Join(home, claudeConfigDir)
}
