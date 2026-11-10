package bridge

import (
	"errors"
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

	// The destination sidecar is durable before the message is visible: a
	// courier that finds the .md always finds the alias it was enqueued for,
	// never the courier's default. The exclusive sidecar also claims the
	// message id against an enqueue for another destination. A failed or
	// crashed enqueue leaves the sidecar in place, never a message without
	// one: a retry for the same destination reuses it, and a concurrent
	// enqueue of the same message cannot lose it.
	target := filepath.Join(spoolDir, filename)
	if err := claimDestSidecar(target, destAlias); err != nil {
		return EnqueueResult{}, err
	}
	if err := syncDirectory(spoolDir); err != nil {
		return EnqueueResult{}, fmt.Errorf("sync bridge spool: %w", err)
	}
	if err := publishSpoolMessage(filepath.Join(filepath.Dir(spoolDir), "tmp"), target, message); err != nil {
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

// publishSpoolMessage writes message under tmpDir and links it to target
// without replacing an existing file, so a reader sees the complete message
// or none of it.
func publishSpoolMessage(tmpDir, target string, message []byte) error {
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("create bridge spool tmp: %w", err)
	}
	file, err := os.CreateTemp(tmpDir, filepath.Base(target)+".*")
	if err != nil {
		return fmt.Errorf("create spool file: %w", err)
	}
	tmp := file.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := file.Write(message); err != nil {
		_ = file.Close()
		return fmt.Errorf("write spool file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync spool file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close spool file: %w", err)
	}
	if err := os.Link(tmp, target); err != nil {
		return fmt.Errorf("publish spool file: %w", err)
	}
	return nil
}

// claimDestSidecar writes the sidecar for target. An existing sidecar with
// exactly the bytes this enqueue would write, beside an unpublished message,
// is left by an earlier attempt of the same enqueue and is reused; any other
// existing sidecar refuses.
func claimDestSidecar(target, destAlias string) error {
	err := writeDestSidecar(target, destAlias)
	if err == nil || !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, readErr := fsq.ReadRegularNoFollow(DestSidecarPath(target))
	if readErr != nil {
		return fmt.Errorf("read existing dest sidecar: %w", readErr)
	}
	if string(existing) != destAlias+"\n" {
		return fmt.Errorf("dest sidecar %q names another destination", DestSidecarPath(target))
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		return fmt.Errorf("spool message %q is already enqueued", target)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat spool message: %w", statErr)
	}
	// The courier archives a sent message before it removes the sidecar, so
	// a sidecar beside an archived copy is a sent message, not a retry.
	sent := filepath.Join(filepath.Dir(filepath.Dir(target)), "sent", filepath.Base(target))
	if _, statErr := os.Lstat(sent); statErr == nil {
		return fmt.Errorf("spool message %q was already sent", target)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stat sent archive: %w", statErr)
	}
	return nil
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
