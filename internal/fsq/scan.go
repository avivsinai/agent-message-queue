package fsq

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	sort.Strings(out)
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
	seen := make(map[string]struct{})
	addMatch := func(path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		matches = append(matches, path)
	}
	scanDir := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
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
				addMatch(filepath.Join(dir, entry.Name()))
			}
		}
		return nil
	}
	for _, agent := range agents {
		if err := scanDir(AgentInboxTmp(root, agent)); err != nil {
			return nil, err
		}
		if err := scanDir(AgentDLQTmp(root, agent)); err != nil {
			return nil, err
		}
		agentDir := filepath.Join(root, "agents", agent)
		if err := filepath.WalkDir(agentDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if !strings.HasPrefix(name, ".") || !strings.Contains(name, ".tmp-") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				addMatch(path)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return matches, nil
}
