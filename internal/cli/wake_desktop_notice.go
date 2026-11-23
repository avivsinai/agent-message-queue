package cli

import "fmt"

const (
	wakeDesktopNoticeTitle = "AMQ"

	wakeDialogGenericHoldReason = "a harness dialog may be open; answer it in that terminal to release"
)

// wakeHeldNoticeBody names the agent, the waiting messages, and why input is
// held.
func wakeHeldNoticeBody(handle string, pending int, reason string) string {
	noun := "messages"
	if pending == 1 {
		noun = "message"
	}
	return fmt.Sprintf("%s: %d %s waiting, doorbell held. %s", handle, pending, noun, reason)
}
