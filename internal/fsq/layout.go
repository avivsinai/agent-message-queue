package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path helpers for standard mailbox directories.

func AgentBase(root, agent string) string {
	return filepath.Join(root, "agents", agent)
}

func AgentInboxTmp(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "tmp")
}

func AgentInboxNew(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "new")
}

func AgentInboxCur(root, agent string) string {
	return filepath.Join(root, "agents", agent, "inbox", "cur")
}

func AgentOutboxSent(root, agent string) string {
	return filepath.Join(root, "agents", agent, "outbox", "sent")
}

func AgentAcksReceived(root, agent string) string {
	return filepath.Join(root, "agents", agent, "acks", "received")
}

func AgentAcksSent(root, agent string) string {
	return filepath.Join(root, "agents", agent, "acks", "sent")
}

func EnsureRootDirs(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "agents"),
		filepath.Join(root, "threads"),
		filepath.Join(root, "meta"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func EnsureAgentDirs(root, agent string) error {
	if strings.TrimSpace(agent) == "" {
		return fmt.Errorf("agent handle is empty")
	}
	dirs := []string{
		AgentInboxTmp(root, agent),
		AgentInboxNew(root, agent),
		AgentInboxCur(root, agent),
		AgentOutboxSent(root, agent),
		AgentAcksReceived(root, agent),
		AgentAcksSent(root, agent),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
