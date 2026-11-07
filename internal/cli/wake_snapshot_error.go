package cli

import "github.com/avivsinai/agent-message-queue/internal/wakelock"

type wakeSnapshotReadChangedError = wakelock.SnapshotReadChangedError

func newWakeSnapshotReadChangedError(err error) error {
	return wakelock.NewSnapshotReadChangedError(err)
}
