package cli

import (
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/followup"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// promoteFollowUps delivers me's due follow-up notices before a command
// reads the inbox. A failure is a warning and never changes the command's
// outcome; the timers stay for the next pass.
func promoteFollowUps(root *fsq.DeliveryRoot, me string) {
	if err := followup.Promote(root, me, time.Now()); err != nil {
		_ = writeStderr("warning: %v\n", err)
	}
}

// promoteFollowUpsAtPath is promoteFollowUps for a process that holds only
// the root path. A mailbox with no follow-ups directory costs one lstat.
func promoteFollowUpsAtPath(root, me string, now time.Time) error {
	if _, err := os.Lstat(fsq.AgentFollowUps(root, me)); os.IsNotExist(err) {
		return nil
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	return followup.Promote(deliveryRoot, me, now)
}
