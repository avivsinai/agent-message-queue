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
	paths, err := DeliverToInboxes(root, []string{agent}, filename, data)
	if err != nil {
		return "", err
	}
	return paths[agent], nil
}

type stagedDelivery struct {
	recipient string
	tmpDir    string
	newDir    string
	tmpPath   string
	newPath   string
}

// DeliverToInboxes writes a message to multiple inboxes.
// On failure, it attempts to roll back any prior deliveries.
func DeliverToInboxes(root string, recipients []string, filename string, data []byte) (map[string]string, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients provided")
	}
	stages := make([]stagedDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		tmpDir := AgentInboxTmp(root, recipient)
		newDir := AgentInboxNew(root, recipient)
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := os.MkdirAll(newDir, 0o755); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		tmpPath := filepath.Join(tmpDir, filename)
		newPath := filepath.Join(newDir, filename)
		if err := writeAndSync(tmpPath, data, 0o644); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := SyncDir(tmpDir); err != nil {
			return nil, cleanupStagedTmp(stages, cleanupTemp(tmpPath, err))
		}
		stages = append(stages, stagedDelivery{
			recipient: recipient,
			tmpDir:    tmpDir,
			newDir:    newDir,
			tmpPath:   tmpPath,
			newPath:   newPath,
		})
	}

	for i, stage := range stages {
		if err := os.Rename(stage.tmpPath, stage.newPath); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				err = fmt.Errorf("rename tmp->new for %s: different filesystems: %w", stage.recipient, err)
			} else {
				err = fmt.Errorf("rename tmp->new for %s: %w", stage.recipient, err)
			}
			return nil, rollbackDeliveries(stages[:i], stages[i:], err)
		}
		if err := SyncDir(stage.newDir); err != nil {
			return nil, rollbackDeliveries(stages[:i+1], stages[i+1:], fmt.Errorf("sync new dir for %s: %w", stage.recipient, err))
		}
	}

	paths := make(map[string]string, len(stages))
	for _, stage := range stages {
		paths[stage.recipient] = stage.newPath
	}
	return paths, nil
}

func cleanupStagedTmp(stages []stagedDelivery, primary error) error {
	var cleanupErr error
	for _, stage := range stages {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
}

func rollbackDeliveries(committed, pending []stagedDelivery, primary error) error {
	var cleanupErr error
	for _, stage := range committed {
		if err := os.Remove(stage.newPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("rollback new %s: %w", stage.newPath, err))
		}
		if err := SyncDir(stage.newDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync new dir %s: %w", stage.newDir, err))
		}
	}
	for _, stage := range pending {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (rollback: %v)", primary, cleanupErr)
}
