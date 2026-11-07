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
// It does not write the evidence of pruned messages, which the commit will
// not take.
func (s Store) stageEvidence(ctx context.Context, blobs [][]byte, messages []Message) (map[string]bool, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	// Validate initialized state before creating even an evidence lock file.
	state, err := s.ReadContext(ctx)
	if err != nil {
		return nil, err
	}
	pruned := map[string]bool{}
	for _, m := range messages {
		if state.Pruned(m.ID) {
			pruned[m.EvidenceHash] = true
		}
	}
	path := filepath.Join(s.Dir, "evidence.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("unsafe evidence budget lock")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var written map[string]bool
	err = lock.WithExclusiveFileLockUntil(path, func() bool { return ctx.Err() != nil }, func() error {
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
		if (identity.Schema != SnapshotSchema && identity.Schema != v2SnapshotSchema && identity.Schema != legacySnapshotSchema) || identity.BaseRoot != s.BaseRoot || identity.BaseRootID != s.BaseRootID || identity.HostID != s.HostID || identity.ProjectID == "" || identity.Generation == 0 {
			return errors.New("evidence staging identity invalid; recovery required")
		}

		if identity.ProjectID != s.ProjectID {
			return ErrProjectChanged
		}
		written, err = s.saveEvidence(ctx, blobs, pruned)
		return err
	})
	return written, err
}
