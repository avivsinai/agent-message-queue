//go:build linux

package cli

import "fmt"

func newWakeClaudeNative(_ *wakeStart, _ *wakeConfig) (wakeNativeNotifier, error) {
	return nil, fmt.Errorf("claude-native requires macOS")
}
