package fsq

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type writeSyncStub struct {
	writeN    int
	writeErr  error
	syncErr   error
	syncCalls int
}

func (s *writeSyncStub) Write(data []byte) (int, error) {
	return s.writeN, s.writeErr
}

func (s *writeSyncStub) Sync() error {
	s.syncCalls++
	return s.syncErr
}

func TestWriteAllAndSyncReturnsShortWrite(t *testing.T) {
	writer := &writeSyncStub{writeN: 3}

	err := writeAllAndSync(writer, []byte("hello"))

	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeAllAndSync() error = %v, want io.ErrShortWrite", err)
	}
	if writer.syncCalls != 0 {
		t.Fatalf("Sync called %d times, want 0", writer.syncCalls)
	}
}

func TestWriteAllAndSyncSyncsAfterFullWrite(t *testing.T) {
	writer := &writeSyncStub{writeN: len("hello")}

	err := writeAllAndSync(writer, []byte("hello"))

	if err != nil {
		t.Fatalf("writeAllAndSync() error = %v, want nil", err)
	}
	if writer.syncCalls != 1 {
		t.Fatalf("Sync called %d times, want 1", writer.syncCalls)
	}
}

func TestWriteFileAtomicReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()

	finalPath, err := WriteFileAtomic(dir, "state.json", []byte("old"), 0o600)
	if err != nil {
		t.Fatalf("initial WriteFileAtomic: %v", err)
	}
	if _, err := WriteFileAtomic(dir, "state.json", []byte("new"), 0o600); err != nil {
		t.Fatalf("replacement WriteFileAtomic: %v", err)
	}

	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("final data = %q, want new", data)
	}
	tmpMatches, err := filepath.Glob(filepath.Join(dir, ".state.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(tmpMatches) != 0 {
		t.Fatalf("temporary files remain: %v", tmpMatches)
	}
}
