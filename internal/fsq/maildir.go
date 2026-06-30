package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// PartialDeliveryError reports the delivery state after a multi-recipient
// delivery fails during the tmp -> new commit phase.
type PartialDeliveryError struct {
	Delivered map[string]string
	Failed    string
	Pending   []string
	Err       error
}

func (e *PartialDeliveryError) Error() string {
	delivered := "none"
	if len(e.Delivered) > 0 {
		recipients := make([]string, 0, len(e.Delivered))
		for recipient := range e.Delivered {
			recipients = append(recipients, recipient)
		}
		sort.Strings(recipients)
		delivered = strings.Join(recipients, ", ")
	}

	failed := e.Failed
	if failed == "" {
		failed = "unknown"
	}

	pending := "none"
	if len(e.Pending) > 0 {
		pending = strings.Join(e.Pending, ", ")
	}

	if e.Err == nil {
		return fmt.Sprintf("partial delivery: delivered to %s; failed for %s; pending %s", delivered, failed, pending)
	}
	return fmt.Sprintf("partial delivery: delivered to %s; failed for %s; pending %s: %v", delivered, failed, pending, e.Err)
}

func (e *PartialDeliveryError) Unwrap() error {
	return e.Err
}

// DeliverToInboxes writes a message to multiple inboxes.
// On partial failure, committed deliveries remain in new/ and undelivered tmp
// files are removed.
func DeliverToInboxes(root string, recipients []string, filename string, data []byte) (map[string]string, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients provided")
	}
	stages := make([]stagedDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		tmpDir := AgentInboxTmp(root, recipient)
		newDir := AgentInboxNew(root, recipient)
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(stages, err)
		}
		tmpPath := filepath.Join(tmpDir, filename)
		newPath := filepath.Join(newDir, filename)
		if err := writeAndSync(tmpPath, data, 0o600); err != nil {
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
			return nil, partialDeliveryError(stages[:i], stage, stages[i+1:], err)
		}
		// Sync both directories after rename for fully durable delivery:
		// - newDir: new entry is visible
		// - tmpDir: old entry removal is durable
		if err := SyncDir(stage.newDir); err != nil {
			return nil, partialDeliveryError(stages[:i+1], stage, stages[i+1:], fmt.Errorf("sync new dir for %s: %w", stage.recipient, err))
		}
		// Best-effort sync of tmpDir (non-fatal: message is already delivered)
		_ = SyncDir(stage.tmpDir)
	}

	paths := make(map[string]string, len(stages))
	for _, stage := range stages {
		paths[stage.recipient] = stage.newPath
	}
	return paths, nil
}

// DeliverToExistingInbox delivers a message to a foreign root's inbox using
// Maildir semantics (tmp -> new). Unlike DeliverToInboxes, it never creates
// directories — the target inbox must already exist. This prevents a sender
// from accidentally scaffolding structure in a peer project.
func DeliverToExistingInbox(root, agent, filename string, data []byte) (string, error) {
	tmpDir := AgentInboxTmp(root, agent)
	newDir := AgentInboxNew(root, agent)

	// Verify directories exist (never create in foreign roots).
	if !dirExists(tmpDir) {
		return "", fmt.Errorf("peer inbox tmp dir does not exist: %s", tmpDir)
	}
	if !dirExists(newDir) {
		return "", fmt.Errorf("peer inbox new dir does not exist: %s", newDir)
	}

	tmpPath := filepath.Join(tmpDir, filename)
	newPath := filepath.Join(newDir, filename)

	if err := writeAndSync(tmpPath, data, 0o600); err != nil {
		return "", err
	}
	if err := SyncDir(tmpDir); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := os.Rename(tmpPath, newPath); err != nil {
		return "", cleanupTemp(tmpPath, fmt.Errorf("rename tmp->new for %s: %w", agent, err))
	}
	if err := SyncDir(newDir); err != nil {
		return "", fmt.Errorf("sync new dir for %s: %w", agent, err)
	}
	_ = SyncDir(tmpDir) // best-effort
	return newPath, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func cleanupStagedTmp(stages []stagedDelivery, primary error) error {
	cleanupErr := cleanupTmpStages(stages)
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
}

func partialDeliveryError(committed []stagedDelivery, failed stagedDelivery, pending []stagedDelivery, primary error) error {
	delivered := make(map[string]string, len(committed))
	for _, stage := range committed {
		delivered[stage.recipient] = stage.newPath
	}

	pendingRecipients := make([]string, 0, len(pending))
	for _, stage := range pending {
		pendingRecipients = append(pendingRecipients, stage.recipient)
	}

	undelivered := make([]stagedDelivery, 0, 1+len(pending))
	undelivered = append(undelivered, failed)
	undelivered = append(undelivered, pending...)
	if cleanupErr := cleanupTmpStages(undelivered); cleanupErr != nil {
		primary = fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
	}

	return &PartialDeliveryError{
		Delivered: delivered,
		Failed:    failed.recipient,
		Pending:   pendingRecipients,
		Err:       primary,
	}
}

func cleanupTmpStages(stages []stagedDelivery) error {
	var cleanupErr error
	for _, stage := range stages {
		if err := os.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := SyncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	return cleanupErr
}
