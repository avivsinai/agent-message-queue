// Package agentidentity reads the observation record an agent leaves in its
// mailbox: <delivery root>/agents/<handle>/identity.json. The record says
// which harness last ran under the handle. It is discovery metadata only and
// never an authorization input.
package agentidentity

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// FileName is the record's name inside an agent mailbox directory.
const FileName = "identity.json"

// Schema is the only record schema this package accepts.
const Schema = 1

// maxRecordBytes bounds a read; a valid record is far smaller.
const maxRecordBytes = 16 << 10

// Record is one observation: the last publication wins, so it holds the most
// recently published start or attach, not a history.
type Record struct {
	Schema     int    `json:"schema"`
	Handle     string `json:"handle"`
	Harness    string `json:"harness"`
	Wrapper    string `json:"wrapper,omitempty"`
	ObservedAt string `json:"observed_at"`
}

// Path returns the record path for handle below a delivery root.
func Path(root, handle string) string {
	return filepath.Join(root, "agents", handle, FileName)
}

// Read reads and validates the record for handle below root. A missing file
// passes the lstat error through, so os.IsNotExist reports "no record"; every
// other error means the record exists but cannot be trusted.
func Read(root, handle string) (Record, error) {
	data, err := fsq.ReadRegularNoFollowBounded(Path(root, handle), maxRecordBytes)
	if err != nil {
		return Record{}, err
	}
	return Parse(data, handle)
}

// Parse validates record bytes read for handle by the caller, for callers
// that read through an authenticated root capability.
func Parse(data []byte, handle string) (Record, error) {
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("invalid %s: %w", FileName, err)
	}
	if rec.Schema != Schema {
		return Record{}, fmt.Errorf("invalid %s: schema %d, want %d", FileName, rec.Schema, Schema)
	}
	if rec.Handle != handle {
		return Record{}, fmt.Errorf("invalid %s: handle %q does not match mailbox %q", FileName, rec.Handle, handle)
	}
	if err := fsq.ValidateLegacyHandleForInspection(rec.Harness); err != nil {
		return Record{}, fmt.Errorf("invalid %s: harness: %w", FileName, err)
	}
	if rec.Wrapper != "" {
		if err := fsq.ValidateLegacyHandleForInspection(rec.Wrapper); err != nil {
			return Record{}, fmt.Errorf("invalid %s: wrapper: %w", FileName, err)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, rec.ObservedAt); err != nil {
		return Record{}, fmt.Errorf("invalid %s: observed_at: %w", FileName, err)
	}
	return rec, nil
}
