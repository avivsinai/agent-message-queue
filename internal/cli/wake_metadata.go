package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const maxWakeMetadataFileBytes = 64 * 1024

func readWakeMetadata(file *os.File, label, path string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxWakeMetadataFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(data) > maxWakeMetadataFileBytes {
		return nil, fmt.Errorf("%s %s is too large", label, path)
	}
	return data, nil
}

func writeWakeMetadataFile(path string, data []byte, label string) error {
	dir := filepath.Dir(path)
	if err := validateWakeMetadataDestination(path, label); err != nil {
		return err
	}
	tmp, file, err := createWakeMetadataTempFile(dir, filepath.Base(path), label)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(tmp)
		}
	}()

	if err := writeAllAndSyncWakeMetadata(file, data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s temp file: %w", label, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s temp file: %w", label, err)
	}
	if err := fsq.SyncDir(dir); err != nil {
		return fmt.Errorf("sync %s directory before install: %w", label, err)
	}
	if err := validateWakeMetadataDestination(path, label); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install %s: %w", label, err)
	}
	installed = true
	if err := fsq.SyncDir(dir); err != nil {
		return fmt.Errorf("sync %s directory after install: %w", label, err)
	}
	return nil
}

func validateWakeMetadataDestination(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %s must not be a symlink", label, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %s must be a regular file", label, path)
	}
	return nil
}

func createWakeMetadataTempFile(dir, base, label string) (string, *os.File, error) {
	tmpBase := strings.TrimLeft(base, ".")
	if tmpBase == "" {
		tmpBase = "wake-metadata"
	}
	for attempt := 0; attempt < 1000; attempt++ {
		tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp.%d.%d.%d", tmpBase, os.Getpid(), time.Now().UnixNano(), attempt))
		file, err := openWakeMetadataTempFile(tmp)
		if err == nil {
			if err := file.Chmod(0o600); err != nil {
				_ = file.Close()
				_ = os.Remove(tmp)
				return "", nil, fmt.Errorf("chmod %s temp file: %w", label, err)
			}
			return tmp, file, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", nil, fmt.Errorf("create %s temp file: %w", label, err)
	}
	return "", nil, fmt.Errorf("create %s temp file: too many name collisions", label)
}

func writeAllAndSyncWakeMetadata(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync()
}
