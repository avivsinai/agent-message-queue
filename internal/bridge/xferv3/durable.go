package xferv3

import (
	"fmt"
	"os"
)

func syncFile(f *os.File) error {
	if err := fullSync(f); err != nil {
		return fmt.Errorf("xferv3: sync file: %w", err)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("xferv3: open dir: %w", err)
	}
	err = fullSync(dir)
	closeErr := dir.Close()
	if err != nil {
		return fmt.Errorf("xferv3: sync dir: %w", err)
	}
	return closeErr
}

func writeSyncedFile(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := syncFile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
