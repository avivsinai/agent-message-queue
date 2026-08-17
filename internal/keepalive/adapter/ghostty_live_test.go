package adapter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGhosttyLiveDiscoverProbe(t *testing.T) {
	if os.Getenv("AMQ_GHOSTTY_LIVE") != "1" {
		t.Skip("AMQ_GHOSTTY_LIVE=1 required; run from a shell inside Ghostty")
	}
	skipNonDarwin(t)

	if _, err := ghosttyLiveRun(5*time.Second, ghosttyLiveVersionScript); err != nil {
		t.Skipf("Ghostty AppleScript unavailable: %v", err)
	}
	beforeWindows, err := ghosttyLiveWindowIDs()
	if err != nil {
		t.Skipf("Ghostty list-windows failed: %v", err)
	}
	beforeCount, err := ghosttyLiveTerminalCount()
	if err != nil {
		t.Fatalf("count terminals: %v", err)
	}
	var windowID string
	t.Cleanup(func() {
		listed, listErr := ghosttyLiveWindowIDs()
		if listErr != nil {
			t.Errorf("cleanup list windows: %v", listErr)
			return
		}
		for _, id := range listed {
			if ghosttyContainsID(beforeWindows, id) {
				continue
			}
			if _, closeErr := ghosttyLiveRun(30*time.Second, ghosttyLiveCloseWindowScript, id); closeErr != nil {
				t.Errorf("cleanup close %s: %v", id, closeErr)
			}
		}
		after, countErr := ghosttyLiveTerminalCount()
		if countErr != nil {
			t.Errorf("cleanup terminal count: %v", countErr)
			return
		}
		if after != beforeCount {
			t.Errorf("live terminal count not restored: before=%d after=%d", beforeCount, after)
		}
	})

	created, err := ghosttyLiveRun(30*time.Second, ghosttyLiveNewWindowScript, t.TempDir())
	if err != nil {
		t.Fatalf("new-window: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(created), "|")
	if len(parts) != 3 {
		t.Fatalf("new-window identities %q, want window|tab|terminal", created)
	}
	windowID = strings.TrimSpace(parts[0])
	rawTerminalID := strings.TrimSpace(parts[2])
	if windowID == "" || rawTerminalID == "" {
		t.Fatalf("new-window identities %q", created)
	}
	terminalID := strings.ToUpper(rawTerminalID)
	if _, err := ghosttyLiveRun(5*time.Second, ghosttyLiveFocusScript, rawTerminalID); err != nil {
		t.Fatalf("focus throwaway: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adapter := Ghostty{}
	target, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	gotID, err := parseGhosttyTerminalTarget(target)
	if err != nil {
		t.Fatalf("Discover target %q: %v", target, err)
	}
	if gotID != terminalID {
		t.Fatalf("Discover = %q, want throwaway %q (refusing to touch another terminal)", target, terminalID)
	}
	t.Logf("PASS Discover throwaway %s", target)
	if err := adapter.Probe(ctx, target); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	t.Logf("PASS Probe throwaway %s", target)

	if _, err := ghosttyLiveRun(30*time.Second, ghosttyLiveCloseWindowScript, windowID); err != nil {
		t.Fatalf("close throwaway: %v", err)
	}
	if err := adapter.Probe(ctx, target); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe after close error = %v, want ErrTargetNotFound", err)
	}
	t.Logf("PASS Probe after close is not-found")
}

func ghosttyContainsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func ghosttyLiveTerminalCount() (int, error) {
	out, err := ghosttyLiveRun(5*time.Second, `tell application "Ghostty" to count terminals`)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func ghosttyLiveWindowIDs() ([]string, error) {
	out, err := ghosttyLiveRun(5*time.Second, ghosttyLiveListWindowsScript)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, ","), nil
}

func ghosttyLiveRun(timeout time.Duration, script string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmdArgs := append([]string{"-e", script}, args...)
	cmd := exec.CommandContext(ctx, "osascript", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), errors.New(strings.TrimSpace(string(out) + ": " + err.Error()))
	}
	return strings.TrimSpace(string(out)), nil
}

const ghosttyLiveVersionScript = `tell application "Ghostty" to get version`

const ghosttyLiveNewWindowScript = `
on run argv
	set cwd to item 1 of argv
	tell application "Ghostty"
		set cfg to new surface configuration
		set initial working directory of cfg to cwd
		set win to new window with configuration cfg
		set term to terminal 1 of selected tab of win
		return (id of win) & "|" & (id of selected tab of win) & "|" & (id of term)
	end tell
end run
`

const ghosttyLiveListWindowsScript = `
tell application "Ghostty"
	set ids to {}
	repeat with w in windows
		set end of ids to id of w
	end repeat
	set AppleScript's text item delimiters to ","
	return ids as text
end tell
`

const ghosttyLiveFocusScript = `
on run argv
	tell application "Ghostty"
		set matches to terminals whose id is (item 1 of argv)
		if (count of matches) is not 1 then error "ghostty terminal not unique"
		focus (item 1 of matches)
	end tell
end run
`

const ghosttyLiveCloseWindowScript = `
on run argv
	set winID to item 1 of argv
	tell application "Ghostty"
		set wins to windows whose id is winID
		if (count of wins) is 0 then error "ghostty window is absent"
		close window (item 1 of wins)
	end tell
end run
`
