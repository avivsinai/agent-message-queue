package swarm

import (
	"os"
	"path/filepath"
)

// Default Claude Code teams and tasks directories.
const (
	claudeConfigDir  = ".claude"
	teamsSubdir      = "teams"
	tasksSubdir      = "tasks"
	teamConfigFile   = "config.json"
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
		return filepath.Join(".", claudeConfigDir)
	}
	return filepath.Join(home, claudeConfigDir)
}
