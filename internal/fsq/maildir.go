package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// DeliverToInbox writes a message using Maildir semantics (tmp -> new).
// It returns the final path in inbox/new.
func DeliverToInbox(root, agent, filename string, data []byte) (string, error) {
	tmpDir := AgentInboxTmp(root, agent)
	newDir := AgentInboxNew(root, agent)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return "", err
	}
	tmpPath := filepath.Join(tmpDir, filename)
	finalPath := filepath.Join(newDir, filename)

	if err := writeAndSync(tmpPath, data, 0o644); err != nil {
		return "", err
	}
	if err := SyncDir(tmpDir); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return "", cleanupTemp(tmpPath, fmt.Errorf("rename tmp->new: different filesystems: %w", err))
		}
		return "", cleanupTemp(tmpPath, fmt.Errorf("rename tmp->new: %w", err))
	}
	if err := SyncDir(newDir); err != nil {
		return "", err
	}
	return finalPath, nil
}
