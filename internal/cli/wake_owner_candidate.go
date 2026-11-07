package cli

import (
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/wakelock"
)

// WakeOwner is the candidate process `coop exec` captured for handle under
// root; see wakelock.ReadOwner.
func WakeOwner(root, handle string) (launch.HolderIdentity, bool) {
	return wakelock.ReadOwner(root, handle)
}
