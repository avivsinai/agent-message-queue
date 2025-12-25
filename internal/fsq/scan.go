package fsq

import (
	"os"
	"path/filepath"
	"time"
)

func ListAgents(root string) ([]string, error) {
	agentsDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

func FindTmpFilesOlderThan(root string, cutoff time.Time) ([]string, error) {
	agents, err := ListAgents(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	matches := []string{}
	for _, agent := range agents {
		tmpDir := AgentInboxTmp(root, agent)
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue // skip unreadable files instead of failing entire scan
			}
			if info.ModTime().Before(cutoff) {
				matches = append(matches, filepath.Join(tmpDir, entry.Name()))
			}
		}
	}
	return matches, nil
}
