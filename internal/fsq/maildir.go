package fsq

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DeliverToInbox writes a message using Maildir semantics (tmp -> new).
// It returns the final path in inbox/new.
func DeliverToInbox(root *DeliveryRoot, agent, filename string, data []byte) (string, error) {
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
		failed = "none"
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
func DeliverToInboxes(root *DeliveryRoot, recipients []string, filename string, data []byte) (map[string]string, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients provided")
	}
	if err := ValidateMessageFilename(filename); err != nil {
		return nil, err
	}
	for _, recipient := range recipients {
		if err := ValidateHandle(recipient); err != nil {
			return nil, err
		}
	}
	if err := root.VerifyBase(); err != nil {
		return nil, err
	}
	stages := make([]stagedDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		tmpDir := filepath.Join("agents", recipient, "inbox", "tmp")
		newDir := filepath.Join("agents", recipient, "inbox", "new")
		if err := root.root.MkdirAll(tmpDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(root, stages, err)
		}
		if err := root.root.MkdirAll(newDir, 0o700); err != nil {
			return nil, cleanupStagedTmp(root, stages, err)
		}
		tmpPath, err := uniqueAttemptTmpPath(tmpDir, filename)
		if err != nil {
			return nil, cleanupStagedTmp(root, stages, err)
		}
		newPath := filepath.Join(newDir, filename)
		if err := root.writeAndSync(tmpPath, data, 0o600); err != nil {
			return nil, cleanupStagedTmp(root, stages, err)
		}
		if err := root.syncDir(tmpDir); err != nil {
			return nil, cleanupStagedTmp(root, stages, root.cleanupTemp(tmpPath, err))
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
		if err := root.publishTmpNoReplace(stage.tmpPath, stage.newPath, data); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				err = fmt.Errorf("rename tmp->new for %s: different filesystems: %w", stage.recipient, err)
			} else {
				err = fmt.Errorf("rename tmp->new for %s: %w", stage.recipient, err)
			}
			return nil, partialDeliveryError(root, stages[:i], stage, stages[i+1:], err)
		}
		// Sync both directories after rename for fully durable delivery:
		// - newDir: new entry is visible
		// - tmpDir: old entry removal is durable
		if err := root.syncDir(stage.newDir); err != nil {
			commitErr := &CommittedDurabilityError{
				FinalPath: root.displayPath(stage.newPath),
				Recipient: stage.recipient,
				Err:       fmt.Errorf("sync new dir: %w", err),
			}
			return nil, committedDeliveryError(root, stages[:i+1], stages[i+1:], commitErr)
		}
		// Best-effort sync of tmpDir (non-fatal: message is already delivered)
		_ = root.syncDir(stage.tmpDir)
	}

	paths := make(map[string]string, len(stages))
	for _, stage := range stages {
		paths[stage.recipient] = root.displayPath(stage.newPath)
	}
	return paths, nil
}

// DeliverToExistingInbox delivers a message to a foreign root's inbox using
// Maildir semantics (tmp -> new). Unlike DeliverToInboxes, it never creates
// directories — the target inbox must already exist. This prevents a sender
// from accidentally scaffolding structure in a peer project.
func DeliverToExistingInbox(root *DeliveryRoot, agent, filename string, data []byte) (string, error) {
	if err := ValidateHandle(agent); err != nil {
		return "", err
	}
	if err := ValidateMessageFilename(filename); err != nil {
		return "", err
	}
	if err := root.VerifyBase(); err != nil {
		return "", err
	}
	if err := ValidateExistingMailboxLayout(root, agent); err != nil {
		return "", err
	}
	tmpDir := filepath.Join("agents", agent, "inbox", "tmp")
	newDir := filepath.Join("agents", agent, "inbox", "new")

	// Verify directories exist (never create in foreign roots).
	if !root.dirExists(tmpDir) {
		return "", fmt.Errorf("peer inbox tmp dir does not exist: %s", root.displayPath(tmpDir))
	}
	if !root.dirExists(newDir) {
		return "", fmt.Errorf("peer inbox new dir does not exist: %s", root.displayPath(newDir))
	}

	tmpPath, err := uniqueAttemptTmpPath(tmpDir, filename)
	if err != nil {
		return "", err
	}
	newPath := filepath.Join(newDir, filename)

	if err := root.writeAndSync(tmpPath, data, 0o600); err != nil {
		return "", err
	}
	if err := root.syncDir(tmpDir); err != nil {
		return "", root.cleanupTemp(tmpPath, err)
	}
	if err := root.publishTmpNoReplace(tmpPath, newPath, data); err != nil {
		return "", fmt.Errorf("rename tmp->new for %s: %w", agent, err)
	}
	committedPath := root.displayPath(newPath)
	if err := root.syncDir(newDir); err != nil {
		return committedPath, &CommittedDurabilityError{
			FinalPath: committedPath,
			Recipient: agent,
			Err:       fmt.Errorf("sync new dir: %w", err),
		}
	}
	_ = root.syncDir(tmpDir) // best-effort
	return committedPath, nil
}

func cleanupStagedTmp(root *DeliveryRoot, stages []stagedDelivery, primary error) error {
	cleanupErr := cleanupTmpStages(root, stages)
	if cleanupErr == nil {
		return primary
	}
	return fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
}

func partialDeliveryError(root *DeliveryRoot, committed []stagedDelivery, failed stagedDelivery, pending []stagedDelivery, primary error) error {
	delivered := make(map[string]string, len(committed))
	for _, stage := range committed {
		delivered[stage.recipient] = root.displayPath(stage.newPath)
	}

	pendingRecipients := make([]string, 0, len(pending))
	for _, stage := range pending {
		pendingRecipients = append(pendingRecipients, stage.recipient)
	}

	// A no-replace collision must keep the failed attempt so both copies survive.
	undelivered := pending
	if !errors.Is(primary, os.ErrExist) {
		undelivered = make([]stagedDelivery, 0, 1+len(pending))
		undelivered = append(undelivered, failed)
		undelivered = append(undelivered, pending...)
	}
	if cleanupErr := cleanupTmpStages(root, undelivered); cleanupErr != nil {
		primary = fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
	}

	return &PartialDeliveryError{
		Delivered: delivered,
		Failed:    failed.recipient,
		Pending:   pendingRecipients,
		Err:       primary,
	}
}

func committedDeliveryError(root *DeliveryRoot, committed, pending []stagedDelivery, primary error) error {
	delivered := make(map[string]string, len(committed))
	for _, stage := range committed {
		delivered[stage.recipient] = root.displayPath(stage.newPath)
	}
	pendingRecipients := make([]string, 0, len(pending))
	for _, stage := range pending {
		pendingRecipients = append(pendingRecipients, stage.recipient)
	}
	if cleanupErr := cleanupTmpStages(root, pending); cleanupErr != nil {
		primary = fmt.Errorf("%w (cleanup: %v)", primary, cleanupErr)
	}
	return &PartialDeliveryError{
		Delivered: delivered,
		Pending:   pendingRecipients,
		Err:       primary,
	}
}

func cleanupTmpStages(root *DeliveryRoot, stages []stagedDelivery) error {
	var cleanupErr error
	for _, stage := range stages {
		if err := root.root.Remove(stage.tmpPath); err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup tmp %s: %w", stage.tmpPath, err))
		}
		if err := root.syncDir(stage.tmpDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync tmp dir %s: %w", stage.tmpDir, err))
		}
	}
	return cleanupErr
}

func uniqueAttemptTmpPath(tmpDir, filename string) (string, error) {
	name, err := uniqueAttemptTmpName(filename)
	if err != nil {
		return "", err
	}
	return filepath.Join(tmpDir, name), nil
}

func uniqueAttemptTmpName(filename string) (string, error) {
	var b [6]byte
	if _, err := readRandom(b[:]); err != nil {
		return "", fmt.Errorf("generate unique tmp name: %w", err)
	}
	return fmt.Sprintf(".%s.tmp-%d-%d-%s", filename, time.Now().UnixNano(), os.Getpid(), hex.EncodeToString(b[:])), nil
}

func attemptTmpMatches(filename, name string) bool {
	if name == filename {
		return true
	}
	return strings.HasPrefix(name, "."+filename+".tmp-")
}

func (r *DeliveryRoot) publishTmpNoReplace(tmpPath, newPath string, data []byte) error {
	if err := r.renameNoReplace(tmpPath, newPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return r.resolvePublishCollision(tmpPath, newPath, data, err)
		}
		return r.cleanupTemp(tmpPath, err)
	}
	return nil
}

func (r *DeliveryRoot) resolvePublishCollision(tmpPath, newPath string, data []byte, renameErr error) error {
	existing, err := r.ReadRegularNoFollow(newPath)
	if err != nil {
		return fmt.Errorf("%w (inspect dest: %v)", renameErr, err)
	}
	if bytes.Equal(existing, data) {
		if removeErr := r.root.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("remove idempotent tmp after collision: %w", removeErr)
		}
		return nil
	}
	return renameErr
}
