package fsq

import (
	"errors"
	"io"
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
