package adapter

import (
	"context"
	"fmt"
	"strings"
)

const claudeDesktopTarget = "claude-desktop:new"

// ClaudeDesktop describes the honest deep-link seat from the wake capability
// ADR: prefilled + new + requires_human. The existing inject-via interface
// cannot claim that vector, so this skeleton stays out of DefaultRegistry
// until a capability-aware caller owns the explicit human handoff.
type ClaudeDesktop struct {
	Runner CommandRunner
}

func (ClaudeDesktop) Name() string {
	return "claude-desktop"
}

func (ClaudeDesktop) NormalizeTarget(target string) (string, error) {
	if strings.TrimSpace(target) != claudeDesktopTarget {
		return "", fmt.Errorf("unsupported Claude Desktop target %q; only the new-session seat is stable", target)
	}
	return claudeDesktopTarget, nil
}

func (ClaudeDesktop) Discover(context.Context) (string, error) {
	return "", claudeDesktopGateError()
}

func (ClaudeDesktop) Probe(context.Context, string) error {
	return claudeDesktopGateError()
}

func (ClaudeDesktop) Inject(context.Context, string, string) error {
	return claudeDesktopGateError()
}

func claudeDesktopGateError() error {
	return fmt.Errorf("%w: Claude Desktop deep-link delivery needs a capability-aware human handoff", ErrGUIAdapterNotReady)
}
