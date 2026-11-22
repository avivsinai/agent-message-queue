// Package desktopnotice shows one short desktop notice with the platform's
// own tool. It imports nothing from this module, so any command can use it.
package desktopnotice

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
	// timeout bounds one notice's child process.
	timeout = 2 * time.Second
	// maxBody bounds a notice's title and body in runes.
	maxBody = 240
)

// Notifier returns the platform's desktop notifier, or nil when none exists:
// osascript on macOS, notify-send on Linux when it is installed. Each notice
// is one short child process with argv arguments, bounded by a 2 s timeout
// and reaped before return. Title and body are cleaned of control
// characters and bounded in length.
func Notifier() func(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		return func(title, body string) error {
			script := "display notification " + appleScriptString(body) +
				" with title " + appleScriptString(title)
			return run("osascript", "-e", script)
		}
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return nil
		}
		return func(title, body string) error {
			return run(path, "--", text(title), text(body))
		}
	}
	return nil
}

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s did not finish within %s", name, timeout)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// text removes terminal and Unicode control characters and bounds the
// length.
func text(s string) string {
	s = strings.Map(func(r rune) rune {
		// Every control character (C0, DEL and C1, where 0x9B is a CSI on
		// some terminals) and the line and paragraph separators.
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	if runes := []rune(s); len(runes) > maxBody {
		s = string(runes[:maxBody])
	}
	return strings.TrimSpace(s)
}

// appleScriptString renders s as one AppleScript string literal. Inside the
// quotes only backslash and double quote are special; control characters are
// removed first.
func appleScriptString(s string) string {
	s = text(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
