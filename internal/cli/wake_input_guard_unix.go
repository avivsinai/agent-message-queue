//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/dialogguard"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

const wakeSubmitFile = ".wake.submit"

type wakeSubmit struct {
	Host launch.HolderIdentity `json:"host"`
	TTY  string                `json:"tty"`
	At   time.Time             `json:"at"`
}

func wakeSubmitModeSupported(cursor bool, mode string) bool {
	if cursor {
		return mode == wakeInjectModeRaw || mode == wakeInjectModePaste
	}
	return mode == wakeInjectModeRaw
}

func recordWakeSubmit(dirfd int, dir *wakeAgentDir, in wakeDialogHookInput) {
	cursor := in.cursorReceiver && in.Event == "beforeSubmitPrompt" && in.ConversationID != ""
	claude := !in.cursorReceiver && in.Event == "UserPromptSubmit" && in.SessionID != ""
	if runtime.GOOS != "darwin" || !in.ownIdentity || (!cursor && !claude) || in.AgentID != "" {
		return
	}
	host := wakeDialogHookHost(in.hostPID)
	if host == nil || validateWakeStateAgentDirAt(dirfd, dir) != nil {
		return
	}
	// Darwin materializes a pending tty read time at its first stat. Observe
	// the real slave before stamping, so that the submit's own read cannot
	// appear as later input at the next wake.
	raw, _, err := readWakeLockFileAt(dirfd, filepath.Join(dir.path, wakeLockFileName))
	target := wakeLock{}
	if err != nil || json.Unmarshal(raw, &target) != nil || !wakeSubmitModeSupported(cursor, target.WakeMode) ||
		filepath.Dir(target.TTY) != "/dev" || (!strings.HasPrefix(filepath.Base(target.TTY), "ttys") && target.TTY != "/dev/console") {
		return
	}
	if owner := target.ResumeOwner; owner != nil {
		if launch.LocalHolder(owner.PID, owner.ProcessStart, owner.BootID) != *host {
			return
		}
	} else if !launch.ProcessRunsUnder(target.PID, *host) {
		return
	}
	if info, err := os.Lstat(target.TTY); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return
	}
	raw, err = json.Marshal(wakeSubmit{Host: *host, TTY: target.TTY, At: time.Now().UTC()})
	if err == nil {
		_ = writeWakeRepairMetadataAt(dirfd, dir, wakeSubmitFile, "wake submit stamp", raw, maxWakeMetadataFileBytes)
	}
}

func (s *wakeStart) newInputGuard() *wakeInputGuard {
	if runtime.GOOS != "darwin" || s.requestedOwner == nil || s.injectVia != "" ||
		(s.currentWake.Lock.WakeMode != wakeInjectModeRaw && s.currentWake.Lock.WakeMode != wakeInjectModePaste) {
		return nil
	}
	dir := s.activeAgentDir
	owner := s.requestedOwner
	host := launch.LocalHolder(owner.PID, owner.ProcessStart, owner.BootID)
	started, err := launch.ProcessStartTime(host)
	if err != nil || host.BootID == "" {
		return nil
	}
	tty := s.currentWake.Lock.TTY
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	guard := &wakeInputGuard{
		sample: func() (ttyInputState, error) { return sampleGuardedTTY(tty) },
	}
	if s.harness != "claude" && s.harness != "cursor" || !wakeSubmitModeSupported(s.harness == "cursor", s.currentWake.Lock.WakeMode) {
		guard.installation = func() string { return inputGuardPlain }
		guard.submitTime = func() time.Time { return time.Time{} }
		return guard
	}
	if s.harness == "cursor" {
		hooks, err := cursorHooksPath()
		if err != nil {
			guard.installation = func() string { return inputGuardNeedsCursorHook }
		} else {
			guard.installation = properInputGuardInstallation(hooks, "cursor", cursorSubmitBinary)
		}
	} else {
		guard.installation = claudeInputGuardInstallation(claude.SettingsPath(home))
	}
	guard.submitTime = func() time.Time { return wakeSubmitTime(dir, host, started, tty) }
	return guard
}

func claudeInputGuardInstallation(settings string) func() string {
	return properInputGuardInstallation(settings, "claude", dialogguard.ClaudeSubmitBinary)
}

func properInputGuardInstallation(settings, harness string, submitBinary func(string) string) func() string {
	var previous fsq.FileVersion
	var bin string
	return func() string {
		// Reparse a changed hook file, and notice replacement or removal
		// of its executable even while the settings are unchanged.
		version, settled, err := fsq.StatVersion(settings, time.Now())
		if err != nil {
			if harness == "cursor" {
				return inputGuardNeedsCursorHook
			}
			return inputGuardNeedsHook
		}
		if version != previous || !settled {
			bin = submitBinary(settings)
			previous = version
		}
		if bin != "" {
			if info, err := os.Stat(bin); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				return properInputGuard(harness)
			}
		}
		if harness == "cursor" {
			return inputGuardNeedsCursorHook
		}
		return inputGuardNeedsHook
	}
}

// cursorSubmitBinary names the AMQ hook that confirms a prompt was emptied.
// The older close form supplies no such proof and stays the plain guard.
func cursorSubmitBinary(path string) string {
	commands, err := hookfile.MarkedCursorHookCommandsAt(path, "beforeSubmitPrompt", wakeDialogHookMarker)
	if err != nil || len(commands) != 1 {
		return ""
	}
	match := cursorDialogHookBinRe.FindStringSubmatch(commands[0])
	if match == nil {
		return ""
	}
	bin := strings.ReplaceAll(match[1], `'\''`, `'`)
	if commands[0] != cursorDialogHookCommand(bin, "submit") {
		return ""
	}
	return bin
}

func wakeSubmitTime(dir *wakeAgentDir, host launch.HolderIdentity, started time.Time, tty string) time.Time {
	var stamp wakeSubmit
	err := dir.withFD(func(fd int) error {
		if err := validateWakeStateAgentDirAt(fd, dir); err != nil {
			return err
		}
		raw, _, exists, err := readWakeRepairMetadataAt(fd, wakeSubmitFile, "wake submit stamp", filepath.Join(dir.path, wakeSubmitFile), maxWakeMetadataFileBytes)
		if err != nil || !exists {
			return err
		}
		return json.Unmarshal(raw, &stamp)
	})
	if err != nil || stamp.Host != host || stamp.TTY != tty || !stamp.At.After(started) {
		return time.Time{}
	}
	return stamp.At
}

func sampleGuardedTTY(tty string) (ttyInputState, error) {
	// Both observations must describe the same underlying terminal.
	// The /dev/tty alias has a separate inode and an unreliable atime.
	if tty == "/dev/tty" || !filepath.IsAbs(tty) {
		return ttyInputState{}, os.ErrInvalid
	}
	target, err := os.Stat(tty)
	if err != nil {
		return ttyInputState{}, err
	}
	in, err := os.Stdin.Stat()
	if err != nil || !os.SameFile(in, target) {
		return ttyInputState{}, os.ErrInvalid
	}
	return sampleTTYInputState(os.Stdin.Fd(), os.Stdin.Fd())
}

// startupInputGuard reports what the launched wake observed in its own
// harness environment, including the launch's effective wake settings.
func startupInputGuard(root, me string) string {
	if runtime.GOOS == "linux" {
		return inputGuardLinux
	}
	dir, err := openExistingWakeAgentDir(root, me)
	if err != nil {
		return inputGuardOff
	}
	defer func() { _ = dir.Close() }()
	status := inputGuardOff
	_ = dir.withFD(func(fd int) error {
		lock := inspectWakeLockAt(fd, dir, root, me)
		if applied, ok := readWakeSettingsAppliedAt(fd, dir, lock); ok {
			switch applied.InputGuard {
			case inputGuardNeedsHook, inputGuardNeedsCursorHook, inputGuardPlain:
				status = applied.InputGuard
			default:
				if inputGuardHasSubmit(applied.InputGuard) {
					status = applied.InputGuard
				}
			}
			if applied.Attended {
				if status == inputGuardOff {
					status = "off, attended: on"
				} else {
					status += ", attended: on"
				}
			}
		}
		return nil
	})
	return status
}
