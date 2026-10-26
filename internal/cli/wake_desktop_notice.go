package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
)

const (
	wakeDesktopNoticeTitle   = "AMQ"
	wakeDesktopNoticeTimeout = 2 * time.Second
	// maxWakeDesktopNoticeBody bounds the notice body in runes.
	maxWakeDesktopNoticeBody = 240

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

// wakeDesktopNotifier returns the platform's desktop notifier for a running
// wake, or nil when none exists: osascript on macOS, notify-send on Linux
// when it is installed. Each notice is one short child process with argv
// arguments, bounded by wakeDesktopNoticeTimeout and reaped before return.
func wakeDesktopNotifier() func(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		return func(title, body string) error {
			script := "display notification " + appleScriptString(body) +
				" with title " + appleScriptString(title)
			return runWakeDesktopNotice("osascript", "-e", script)
		}
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return nil
		}
		return func(title, body string) error {
			return runWakeDesktopNotice(path, "--", desktopNoticeText(title), desktopNoticeText(body))
		}
	}
	return nil
}

func runWakeDesktopNotice(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), wakeDesktopNoticeTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s did not finish within %s", name, wakeDesktopNoticeTimeout)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// desktopNoticeText removes terminal and Unicode control characters and
// bounds the length.
func desktopNoticeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, sanitizeForTTY(s))
	if runes := []rune(s); len(runes) > maxWakeDesktopNoticeBody {
		s = string(runes[:maxWakeDesktopNoticeBody])
	}
	return strings.TrimSpace(s)
}

// appleScriptString renders s as one AppleScript string literal. Inside the
// quotes only backslash and double quote are special; control characters are
// removed first.
func appleScriptString(s string) string {
	s = desktopNoticeText(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
