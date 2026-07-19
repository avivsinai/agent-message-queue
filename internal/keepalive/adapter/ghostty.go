package adapter

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Ghostty struct {
	Runner CommandRunner
}

const ghosttyTerminalTargetPrefix = "ghostty:terminal:"

func (Ghostty) Name() string {
	return "ghostty"
}

func (g Ghostty) Discover(ctx context.Context) (string, error) {
	if err := requireDarwin(); err != nil {
		return "", err
	}
	out, err := g.runner().Run(ctx, "osascript", "-e", ghosttyDiscoverScript)
	if err != nil {
		return "", fmt.Errorf("discover Ghostty terminal: %w: %s", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("discover Ghostty terminal: empty terminal id")
	}
	return ghosttyTerminalTargetPrefix + id, nil
}

func (g Ghostty) Probe(ctx context.Context, target string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	id, err := parseGhosttyTerminalTarget(target)
	if err != nil {
		return err
	}
	out, err := g.runner().Run(ctx, "osascript", "-e", ghosttyProbeScript, id)
	if err != nil {
		return fmt.Errorf("probe Ghostty target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (g Ghostty) Inject(ctx context.Context, target string, payload string) error {
	if err := requireDarwin(); err != nil {
		return err
	}
	id, err := parseGhosttyTerminalTarget(target)
	if err != nil {
		return err
	}
	payload = sanitizePayloadForSubmit(payload)
	out, err := g.runner().Run(ctx, "osascript", "-e", ghosttyInjectScript, id, payload)
	if err != nil {
		return fmt.Errorf("inject into Ghostty target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (g Ghostty) runner() CommandRunner {
	if g.Runner != nil {
		return g.Runner
	}
	return ExecRunner{}
}

func requireDarwin() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("ghostty adapter requires macOS, got %s", runtime.GOOS)
	}
	return nil
}

func parseGhosttyTerminalTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("ghostty adapter target is required")
	}
	id, ok := strings.CutPrefix(target, ghosttyTerminalTargetPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported Ghostty target %q; reattach required: run reattach --adapter ghostty to register a terminal-id target", target)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("ghostty terminal target is missing an id")
	}
	return id, nil
}

func sanitizePayloadForSubmit(payload string) string {
	return strings.TrimRight(payload, "\r\n")
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
