package fsq

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DLQSchemaVersion = "amq/dlq/v1"
	MaxRetries       = 3
)

// DLQEnvelope wraps a failed message with failure metadata.
type DLQEnvelope struct {
	Schema        string `json:"schema"`
	ID            string `json:"id"`
	OriginalID    string `json:"original_id"`
	OriginalFile  string `json:"original_file"`
	FailureReason string `json:"failure_reason"`
	FailureDetail string `json:"failure_detail"`
	FailureTime   string `json:"failure_time"`
	RetryCount    int    `json:"retry_count"`
	SourceDir     string `json:"source_dir"`
}

// GenerateDLQID creates a unique ID for a DLQ envelope.
func GenerateDLQID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("dlq_%d_%d_%s", time.Now().UnixNano(), os.Getpid(), hex.EncodeToString(b))
}

// MoveToDLQ moves a failed message from inbox/new to dlq/new with envelope.
func MoveToDLQ(root, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
	if err := MoveNewToCur(root, agent, filename); err != nil {
		return "", fmt.Errorf("claim original: %w", err)
	}
	return moveInboxMessageToDLQ(root, agent, BoxCur, BoxNew, filename, originalID, failureReason, failureDetail)
}

// MoveCurToDLQ moves an already-claimed inbox/cur message to dlq/new.
func MoveCurToDLQ(root, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
	return moveInboxMessageToDLQ(root, agent, BoxCur, BoxCur, filename, originalID, failureReason, failureDetail)
}

func moveInboxMessageToDLQ(root, agent, readDir, envelopeSourceDir, filename, originalID, failureReason, failureDetail string) (string, error) {
	if err := ValidateMessageFilename(filename); err != nil {
		return "", err
	}
	srcDir, err := inboxSourceDir(root, agent, readDir)
	if err != nil {
		return "", err
	}
	srcPath := filepath.Join(srcDir, filename)

	// Read original content
	content, err := ReadRegularNoFollow(srcPath)
	if err != nil {
		return "", fmt.Errorf("read original: %w", err)
	}

	// Create envelope
	envelope := DLQEnvelope{
		Schema:        DLQSchemaVersion,
		ID:            GenerateDLQID(),
		OriginalID:    originalID,
		OriginalFile:  filename,
		FailureReason: failureReason,
		FailureDetail: failureDetail,
		FailureTime:   time.Now().UTC().Format(time.RFC3339),
		RetryCount:    0,
		SourceDir:     envelopeSourceDir,
	}

	// Serialize envelope + original content
	data, err := serializeDLQMessage(envelope, content)
	if err != nil {
		return "", fmt.Errorf("serialize dlq: %w", err)
	}

	// Write to DLQ using atomic delivery (tmp -> new)
	dlqFilename := envelope.ID + ".md"
	dlqPath, err := deliverToDLQ(root, agent, dlqFilename, data)
	if err != nil {
		return "", fmt.Errorf("deliver to dlq: %w", err)
	}

	if err := os.Remove(srcPath); err != nil && !os.IsNotExist(err) {
		return dlqPath, fmt.Errorf("remove original (dlq written): %w", err)
	}
	_ = SyncDir(srcDir)

	return dlqPath, nil
}

func inboxSourceDir(root, agent, sourceDir string) (string, error) {
	switch sourceDir {
	case BoxNew:
		return AgentInboxNew(root, agent), nil
	case BoxCur:
		return AgentInboxCur(root, agent), nil
	default:
		return "", fmt.Errorf("unsupported inbox source dir %q", sourceDir)
	}
}

// deliverToDLQ writes a DLQ message using Maildir semantics (tmp -> new).
func deliverToDLQ(root, agent, filename string, data []byte) (string, error) {
	tmpDir := AgentDLQTmp(root, agent)
	newDir := AgentDLQNew(root, agent)

	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return "", err
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
		return "", cleanupTemp(tmpPath, err)
	}
	if err := SyncDir(newDir); err != nil {
		return "", err
	}
	_ = SyncDir(tmpDir)

	return newPath, nil
}

// ReadDLQEnvelope reads and parses a DLQ message.
func ReadDLQEnvelope(path string) (*DLQEnvelope, []byte, error) {
	data, err := ReadRegularNoFollow(path)
	if err != nil {
		return nil, nil, err
	}

	envelope, body, err := parseDLQMessage(data)
	if err != nil {
		return nil, nil, err
	}

	return envelope, body, nil
}

// RetryFromDLQ moves a message from DLQ back to inbox/new for reprocessing.
// Returns error if retry_count >= MaxRetries and force is false.
func RetryFromDLQ(root, agent, dlqFilename string, force bool) error {
	// Find in dlq/new or dlq/cur
	dlqPath, box, err := FindDLQMessage(root, agent, dlqFilename)
	if err != nil {
		return err
	}

	envelope, originalContent, err := ReadDLQEnvelope(dlqPath)
	if err != nil {
		return fmt.Errorf("read dlq envelope: %w", err)
	}

	if envelope.RetryCount >= MaxRetries && !force {
		return fmt.Errorf("max retries (%d) exceeded; use --force to override", MaxRetries)
	}
	if err := ValidateMessageFilename(envelope.OriginalFile); err != nil {
		return fmt.Errorf("invalid original_file %q: %w", envelope.OriginalFile, err)
	}

	// Check if original file already exists in inbox/new (avoid overwrite)
	inboxNewPath := filepath.Join(AgentInboxNew(root, agent), envelope.OriginalFile)
	if _, err := os.Stat(inboxNewPath); err == nil {
		return fmt.Errorf("original file already exists in inbox/new: %s", envelope.OriginalFile)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat inbox/new original: %w", err)
	}

	envelope.RetryCount++
	updatedData, err := serializeDLQMessage(*envelope, originalContent)
	if err != nil {
		return fmt.Errorf("serialize updated dlq envelope: %w", err)
	}

	if err := updateRetriedDLQEnvelope(root, agent, dlqFilename, dlqPath, box, updatedData); err != nil {
		return err
	}

	// Deliver original content back to inbox only after the DLQ state transition
	// succeeds, so metadata failures cannot duplicate retry delivery.
	if _, err := DeliverToInbox(root, agent, envelope.OriginalFile, originalContent); err != nil {
		return fmt.Errorf("redeliver to inbox: %w", err)
	}

	return nil
}

func updateRetriedDLQEnvelope(root, agent, dlqFilename, dlqPath, box string, updatedData []byte) error {
	curDir := AgentDLQCur(root, agent)
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		return fmt.Errorf("prepare dlq envelope cur dir: %w", err)
	}

	if box == BoxNew {
		// Source is dlq/new: write to dlq/cur atomically, then remove from dlq/new
		if _, err := WriteFileAtomic(curDir, dlqFilename, updatedData, 0o600); err != nil {
			return fmt.Errorf("write updated dlq envelope to cur: %w", err)
		}
		if err := os.Remove(dlqPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old dlq envelope from new: %w", err)
		}
		if err := SyncDir(filepath.Dir(dlqPath)); err != nil {
			return fmt.Errorf("sync old dlq envelope dir: %w", err)
		}
		return SyncDir(curDir)
	}

	// Source is dlq/cur: update in place atomically (same location)
	if _, err := WriteFileAtomic(curDir, dlqFilename, updatedData, 0o600); err != nil {
		return fmt.Errorf("update dlq envelope in cur: %w", err)
	}
	return SyncDir(curDir)
}

// FindDLQMessage locates a DLQ message in dlq/new or dlq/cur.
func FindDLQMessage(root, agent, filename string) (string, string, error) {
	if err := ValidateMessageFilename(filename); err != nil {
		return "", "", err
	}
	newPath := filepath.Join(AgentDLQNew(root, agent), filename)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, BoxNew, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	curPath := filepath.Join(AgentDLQCur(root, agent), filename)
	if _, err := os.Stat(curPath); err == nil {
		return curPath, BoxCur, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
}

// MoveDLQNewToCur moves a DLQ message from new to cur (marks as inspected).
func MoveDLQNewToCur(root, agent, filename string) error {
	if err := ValidateMessageFilename(filename); err != nil {
		return err
	}
	newPath := filepath.Join(AgentDLQNew(root, agent), filename)
	curDir := AgentDLQCur(root, agent)
	curPath := filepath.Join(curDir, filename)
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(newPath, curPath); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(newPath)); err != nil {
		return err
	}
	return SyncDir(curDir)
}

// serializeDLQMessage creates a DLQ file with JSON frontmatter and original content.
func serializeDLQMessage(env DLQEnvelope, originalContent []byte) ([]byte, error) {
	header, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	buf.WriteString("---\n")
	buf.Write(header)
	buf.WriteString("\n---\n")
	buf.Write(originalContent)
	return []byte(buf.String()), nil
}

// parseDLQMessage parses a DLQ file into envelope and original content.
func parseDLQMessage(data []byte) (*DLQEnvelope, []byte, error) {
	// Normalize CRLF to LF for cross-platform compatibility
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, fmt.Errorf("missing frontmatter start")
	}
	rest := data[4:]
	endIdx := bytes.Index(rest, []byte("\n---\n"))
	if endIdx < 0 {
		return nil, nil, fmt.Errorf("missing frontmatter end")
	}

	headerJSON := rest[:endIdx]
	body := rest[endIdx+5:]

	var env DLQEnvelope
	if err := json.Unmarshal(headerJSON, &env); err != nil {
		return nil, nil, fmt.Errorf("parse envelope: %w", err)
	}

	return &env, body, nil
}
