package board

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// stageEvidence never holds the snapshot writer lock. Immutable leftovers are
// safe and count toward capacity; the later commit revalidates every reference.
func (s Store) stageEvidence(ctx context.Context, blobs [][]byte) error {
	if len(blobs) == 0 {
		return nil
	}
	// Validate initialized state before creating even an evidence lock file.
	if _, err := s.ReadContext(ctx); err != nil {
		return err
	}
	path := filepath.Join(s.Dir, "evidence.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe evidence budget lock")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return lock.WithExclusiveFileLockUntil(path, func() bool { return ctx.Err() != nil }, func() error {
		if err := s.validateIdentity(ctx); err != nil {
			return err
		}
		if err := s.safeDirectory(); err != nil {
			return err
		}
		data, err := readBounded(filepath.Join(s.Dir, "snapshot.json"), snapshotLimit)
		if err != nil {
			return err
		}
		var identity struct {
			Schema     string `json:"schema"`
			BaseRoot   string `json:"base_root"`
			BaseRootID string `json:"base_root_id"`
			HostID     string `json:"host_id"`
			ProjectID  string `json:"project_id"`
			Generation uint64 `json:"generation"`
		}
		if err = json.Unmarshal(data, &identity); err != nil {
			return err
		}
		if (identity.Schema != SnapshotSchema && identity.Schema != legacySnapshotSchema) || identity.BaseRoot != s.BaseRoot || identity.BaseRootID != s.BaseRootID || identity.HostID != s.HostID || identity.ProjectID == "" || identity.Generation == 0 {
			return errors.New("evidence staging identity invalid; recovery required")
		}

		if identity.ProjectID != s.ProjectID {
			return ErrProjectChanged
		}

		return s.saveEvidence(ctx, blobs)
	})
}
