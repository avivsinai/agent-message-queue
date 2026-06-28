package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

const ghosttyTerminalTargetPrefix = "ghostty:terminal:"

var runGhosttyCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func discoverGhosttyTerminalID(ctx context.Context) (string, error) {
	if err := requireGhosttyDarwin(); err != nil {
		return "", err
	}
	out, err := runGhosttyCommand(ctx, "osascript", "-e", ghosttyDiscoverScript)
	if err != nil {
		return "", fmt.Errorf("discover Ghostty terminal: %w: %s", err, strings.TrimSpace(string(out)))
	}
	id, err := normalizeGhosttyTerminalID(string(out))
	if err != nil {
		return "", fmt.Errorf("discover Ghostty terminal: %w", err)
	}
	return id, nil
}

func probeGhosttyTerminalID(ctx context.Context, terminalID string) error {
	if err := requireGhosttyDarwin(); err != nil {
		return err
	}
	id, err := normalizeGhosttyTerminalID(terminalID)
	if err != nil {
		return err
	}
	out, err := runGhosttyCommand(ctx, "osascript", "-e", ghosttyProbeScript, id)
	if err != nil {
		return fmt.Errorf("probe Ghostty target %q: %w: %s", ghosttyTargetString(id), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func injectGhostty(ctx context.Context, terminalID string, payload string) error {
	if err := requireGhosttyDarwin(); err != nil {
		return err
	}
	id, err := normalizeGhosttyTerminalID(terminalID)
	if err != nil {
		return err
	}
	payload = strings.TrimRight(payload, "\r\n")
	out, err := runGhosttyCommand(ctx, "osascript", "-e", ghosttyInjectScript, id, payload)
	if err != nil {
		return fmt.Errorf("inject into Ghostty target %q: %w: %s", ghosttyTargetString(id), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func normalizeGhosttyTerminalID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", errors.New("ghostty terminal id is required")
	}
	id = strings.TrimPrefix(id, ghosttyTerminalTargetPrefix)
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("ghostty terminal target is missing an id")
	}
	if strings.ContainsRune(id, 0) {
		return "", errors.New("ghostty terminal id contains NUL")
	}
	return id, nil
}

func ghosttyTargetString(terminalID string) string {
	return ghosttyTerminalTargetPrefix + terminalID
}

func requireGhosttyDarwin() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("ghostty wake requires macOS, got %s", runtime.GOOS)
	}
	return nil
}

const ghosttyDiscoverScript = `
tell application "Ghostty"
	if (count of terminals) is 0 then error "Ghostty has no terminals"
	return id of focused terminal of selected tab of front window
end tell
`

const ghosttyProbeScript = `
on run argv
	set targetID to item 1 of argv
	tell application "Ghostty"
		set matches to terminals whose id is targetID
		set matchCount to count of matches
		if matchCount is 1 then return "ok"
		if matchCount is 0 then error "no Ghostty terminal with id: " & targetID
		error "ambiguous Ghostty terminal id: " & targetID
	end tell
end run
`

const ghosttyInjectScript = `
on run argv
	set targetID to item 1 of argv
	set payload to item 2 of argv
	tell application "Ghostty"
		set matches to terminals whose id is targetID
		set matchCount to count of matches
		if matchCount is 0 then error "no Ghostty terminal with id: " & targetID
		if matchCount is greater than 1 then error "ambiguous Ghostty terminal id: " & targetID
		set targetTerminal to item 1 of matches
		input text payload to targetTerminal
		send key "enter" to targetTerminal
	end tell
end run
`
