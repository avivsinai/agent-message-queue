package adapter

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

const codexAppTargetPrefix = "codex-app:tab:"

// CodexApp is the task-0-gated seat for the native Codex app Apple Events
// surface. The adapter is intentionally a skeleton until the fixed
// execute-javascript probe has been run on a Mac with a window open.
// Task-0 live evidence (Codex app 26.814.41407) returned Access not allowed
// (-1723), and the AllowJavaScriptAppleEvents defaults key was absent. The
// finding is recorded at https://github.com/avivsinai/agent-message-queue/issues/640#issuecomment-5402828566;
// keep this seat refused until an explicit operator decision changes the gate.
//
// The Runner field is reserved for the native osascript implementation after
// the gate is recorded. Keeping the dependency injectable now makes the
// eventual identity and submit tests deterministic without invoking a GUI.
type CodexApp struct {
	Runner CommandRunner
}

func (CodexApp) Name() string {
	return "codex-app"
}

func (CodexApp) NormalizeTarget(target string) (string, error) {
	id, err := parseCodexAppTarget(target)
	if err != nil {
		return "", err
	}
	return codexAppTargetPrefix + id, nil
}

func (CodexApp) Discover(context.Context) (string, error) {
	return "", codexAppGateError()
}

func (CodexApp) Probe(context.Context, string) error {
	return codexAppGateError()
}

func (CodexApp) Inject(context.Context, string, string) error {
	return codexAppGateError()
}

func codexAppGateError() error {
	return fmt.Errorf("%w: codex-app execute-javascript task-0 evidence is required before registration", ErrGUIAdapterNotReady)
}

func parseCodexAppTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	id, ok := strings.CutPrefix(target, codexAppTargetPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported Codex app target %q; reattach required with a pinned tab identity", target)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("codex app target is missing a tab identity")
	}
	for _, char := range id {
		if unicode.IsSpace(char) || unicode.IsControl(char) {
			return "", fmt.Errorf("codex app target contains unsafe tab identity")
		}
	}
	return id, nil
}

// codexAppTask0AppleScript is the fixed native probe represented by
// scripts/probe-codex-app-execute-javascript.sh. It has no payload input and
// only returns a constant from the selected single tab.
const codexAppTask0AppleScript = `
on run
	tell application "ChatGPT"
		set windowCount to count of windows
		if windowCount is not 1 then error "task-0 requires exactly one Codex app window"
		set targetWindow to item 1 of windows
		set tabCount to count of tabs of targetWindow
		if tabCount is not 1 then error "task-0 requires exactly one Codex app tab"
		set targetTab to item 1 of tabs of targetWindow
		return execute javascript "(() => 'AMQ_CODEX_APP_EXECUTE_JS_TASK_0')()" in targetTab
	end tell
end run
`
