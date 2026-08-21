package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// EnqueueResult is the durable local outcome of one bridge enqueue.
type EnqueueResult struct {
	Path     string
	Filename string
}

// Enqueue writes one complete AMQ message into the bridge outbound spool.
func Enqueue(cfg EnqueueConfig, destAlias string, message []byte) (EnqueueResult, error) {
	destAlias = strings.TrimSpace(destAlias)
	if destAlias == "" {
		return EnqueueResult{}, fmt.Errorf("destination alias is required")
	}
	if _, _, err := ParseAlias(destAlias); err != nil {
		return EnqueueResult{}, fmt.Errorf("destination alias: %w", err)
	}
	if _, ok := cfg.AllowedDestSet()[destAlias]; !ok {
		return EnqueueResult{}, fmt.Errorf("destination alias %q is not in the allowlist", destAlias)
	}
	if len(message) == 0 {
		return EnqueueResult{}, fmt.Errorf("message is empty")
	}
	if len(message) > format.MaxMessageSize {
		return EnqueueResult{}, fmt.Errorf("message exceeds maximum size")
	}

	parsed, err := format.ParseMessage(message)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("parse message: %w", err)
	}
	if parsed.Header.ID == "" || strings.TrimSpace(parsed.Header.ID) != parsed.Header.ID {
		return EnqueueResult{}, fmt.Errorf("message id is invalid")
	}
	if parsed.Header.Thread == "" || strings.TrimSpace(parsed.Header.Thread) != parsed.Header.Thread {
		return EnqueueResult{}, fmt.Errorf("message thread is invalid")
	}
	if parsed.Header.From != cfg.SourceHandle {
		return EnqueueResult{}, fmt.Errorf("message sender %q does not match source handle %q", parsed.Header.From, cfg.SourceHandle)
	}

	filename := parsed.Header.ID + ".md"
	if err := fsq.ValidateMessageFilename(filename); err != nil {
		return EnqueueResult{}, fmt.Errorf("message filename: %w", err)
	}

	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("resolve bridge root: %w", err)
	}
	spoolDir := filepath.Join(root, "bridge", "outbox", cfg.SourceHandle, "new")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return EnqueueResult{}, fmt.Errorf("create bridge spool: %w", err)
	}

	target := filepath.Join(spoolDir, filename)
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("create spool file: %w", err)
	}
	if _, writeErr := file.Write(message); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(target)
		return EnqueueResult{}, fmt.Errorf("write spool file: %w", writeErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		_ = file.Close()
		_ = os.Remove(target)
		return EnqueueResult{}, fmt.Errorf("sync spool file: %w", syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(target)
		return EnqueueResult{}, fmt.Errorf("close spool file: %w", closeErr)
	}
	if err := writeDestSidecar(target, destAlias); err != nil {
		_ = os.Remove(target)
		return EnqueueResult{}, err
	}
	if err := syncDirectory(spoolDir); err != nil {
		return EnqueueResult{}, fmt.Errorf("sync bridge spool: %w", err)
	}
	return EnqueueResult{Path: target, Filename: filename}, nil
}

// DestSidecarPath is the sibling dest-binding file for a spool message.
func DestSidecarPath(messagePath string) string {
	return strings.TrimSuffix(messagePath, ".md") + ".dest"
}

func writeDestSidecar(messagePath, destAlias string) error {
	path := DestSidecarPath(messagePath)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create dest sidecar: %w", err)
	}
	if _, writeErr := file.WriteString(destAlias + "\n"); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write dest sidecar: %w", writeErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("sync dest sidecar: %w", syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close dest sidecar: %w", closeErr)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
