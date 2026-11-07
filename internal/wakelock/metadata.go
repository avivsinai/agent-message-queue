package wakelock

import (
	"fmt"
	"io"
	"os"
)

// MaxMetadataFileBytes bounds every wake metadata read.
const MaxMetadataFileBytes = 64 * 1024

// ReadBounded reads at most MaxMetadataFileBytes from file and refuses a
// larger file.
func ReadBounded(file *os.File, label, path string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, MaxMetadataFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(data) > MaxMetadataFileBytes {
		return nil, fmt.Errorf("%s %s is too large", label, path)
	}
	return data, nil
}

// SnapshotReadChangedError marks an observation that could not bind one
// stable file identity. It is retryable only when the caller can restart the
// complete read-only decision without reusing evidence from the failed read.
type SnapshotReadChangedError struct {
	err error
}

func (err *SnapshotReadChangedError) Error() string {
	return err.err.Error()
}

func (err *SnapshotReadChangedError) Unwrap() error {
	return err.err
}

// NewSnapshotReadChangedError wraps err; it returns nil for a nil err.
func NewSnapshotReadChangedError(err error) error {
	if err == nil {
		return nil
	}
	return &SnapshotReadChangedError{err: err}
}
