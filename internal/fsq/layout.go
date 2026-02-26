package fsq

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// handleRe matches valid agent handles: lowercase letters, digits, underscore, hyphen.
var handleRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidateHandle returns an error if the agent handle contains path traversal
// characters or does not match the allowed pattern.
func ValidateHandle(agent string) error {
	if agent == "" || strings.TrimSpace(agent) == "" {
		return fmt.Errorf("agent handle is empty")
	}
	if strings.Contains(agent, "..") || strings.Contains(agent, "/") || strings.Contains(agent, string(filepath.Separator)) {
		return fmt.Errorf("agent handle contains path traversal: %q", agent)
	}
	if !handleRe.MatchString(agent) {
		return fmt.Errorf("agent handle must match [a-z0-9_-]+: %q", agent)
	}
	return nil
}

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

func AgentDLQTmp(root, agent string) string {
	return filepath.Join(root, "agents", agent, "dlq", "tmp")
}

func AgentDLQNew(root, agent string) string {
	return filepath.Join(root, "agents", agent, "dlq", "new")
}

func AgentDLQCur(root, agent string) string {
	return filepath.Join(root, "agents", agent, "dlq", "cur")
}

func EnsureRootDirs(root string) error {
	for _, dir := range []string{
		filepath.Join(root, "agents"),
		filepath.Join(root, "threads"),
		filepath.Join(root, "meta"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTopicName returns an error if the topic name is invalid.
// Topic names follow the same rules as agent handles: [a-z0-9_-]+.
func ValidateTopicName(topic string) error {
	if topic == "" || strings.TrimSpace(topic) == "" {
		return fmt.Errorf("topic name is empty")
	}
	if strings.Contains(topic, "..") || strings.Contains(topic, "/") || strings.Contains(topic, string(filepath.Separator)) {
		return fmt.Errorf("topic name contains path traversal: %q", topic)
	}
	if !handleRe.MatchString(topic) {
		return fmt.Errorf("topic name must match [a-z0-9_-]+: %q", topic)
	}
	return nil
}

// SpecsDir returns the specs directory path: <root>/specs.
func SpecsDir(root string) string {
	return filepath.Join(root, "specs")
}

// SpecTopicDir returns the spec topic directory path: <root>/specs/<topic>.
func SpecTopicDir(root, topic string) string {
	return filepath.Join(root, "specs", topic)
}

// EnsureSpecDirs creates the specs directory tree for a given topic.
func EnsureSpecDirs(root, topic string) error {
	if err := ValidateTopicName(topic); err != nil {
		return err
	}
	dir := SpecTopicDir(root, topic)
	return os.MkdirAll(dir, 0o700)
}

func EnsureAgentDirs(root, agent string) error {
	if err := ValidateHandle(agent); err != nil {
		return err
	}
	dirs := []string{
		AgentInboxTmp(root, agent),
		AgentInboxNew(root, agent),
		AgentInboxCur(root, agent),
		AgentOutboxSent(root, agent),
		AgentAcksReceived(root, agent),
		AgentAcksSent(root, agent),
		AgentDLQTmp(root, agent),
		AgentDLQNew(root, agent),
		AgentDLQCur(root, agent),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}
