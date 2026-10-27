//go:build linux

package cli

import "fmt"

func newWakeClaudeNative(s *wakeStart, _ *wakeConfig) (wakeNativeNotifier, error) {
	if s.injectMode != wakeInjectModeClaudeNative {
		return nil, nil
	}
	return nil, fmt.Errorf("claude-native requires macOS")
}
