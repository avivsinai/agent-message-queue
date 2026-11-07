package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// The machine agent watches the Claude settings file of each session it
// serves, the settings.json in the config_dir its binding records. A file
// whose identity, size and mtime did not change since the last tick costs
// one stat. When a changed file has lost the AMQ approval hook (or, for a
// native session, the Stop hook), the agent puts the hook back through the
// locked settings writer while the owner's opt-in for that file exists, and
// says so once in the session's channel. A hook of an older form it
// upgrades as doctor --fix does while the opt-in exists, and says so, else
// says once that doctor --fix upgrades it; with no opt-in it only says, once
// per change, that the session's approvals (or native turns) now stay in
// the terminal. A removal the owner made with the uninstall commands clears
// the opt-in, so it stays removed.

// settingsSeen is what the machine agent last saw of one settings file:
// its stat, or nil when there was no file.
type settingsSeen struct {
	info os.FileInfo
}

// unchanged reports whether info is the file seen last time.
func (w settingsSeen) unchanged(info os.FileInfo) bool {
	if w.info == nil || info == nil {
		return w.info == nil && info == nil
	}
	return os.SameFile(w.info, info) && w.info.Size() == info.Size() && w.info.ModTime().Equal(info.ModTime())
}

// hookReader is one served session that reads a settings file.
type hookReader struct {
	channel string
	native  bool
}

// watchHooks checks the settings file of each Claude session among the
// bindings all (the ones this tick read) that has an active channel of
// this machine agent and records its config_dir.
func (s *Server) watchHooks(all []binding.Binding) {
	readers := map[string][]hookReader{}
	var order []string
	snap := s.snapshot()
	for _, b := range all {
		if b.ConfigDir == "" || !filepath.IsAbs(b.ConfigDir) || binding.ValidName(b.Name) != nil {
			continue
		}
		r, ok := snap.slot(b.Name)
		if !ok || !routable(r) || !serves(r, b) {
			continue
		}
		path := filepath.Join(filepath.Clean(b.ConfigDir), "settings.json")
		if _, seen := readers[path]; !seen {
			order = append(order, path)
		}
		readers[path] = append(readers[path], hookReader{channel: r.ChannelID, native: !b.Mailbox()})
	}
	if s.machine.settings == nil {
		s.machine.settings = map[string]settingsSeen{}
	}
	for path := range s.machine.settings {
		if _, served := readers[path]; !served {
			delete(s.machine.settings, path)
		}
	}
	for _, path := range order {
		if s.stopped() {
			return
		}
		info := statSettings(path)
		if last, ok := s.machine.settings[path]; ok && last.unchanged(info) {
			continue
		}
		s.machine.settings[path] = settingsSeen{info: info}
		s.checkHooks(path, info, readers[path])
	}
}

// remoteExecutable is the amq-remote a restored hook runs
// (claude.HookBinary). Tests replace it.
var remoteExecutable = func() (string, error) { return claude.HookBinary("amq-remote") }

func statSettings(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// Lines the machine agent posts about a hook that left a settings file.
const (
	hookRestoredLine   = "The AMQ %s hook was missing from %s (changed %s); restored."
	hookUpgradedLine   = "The AMQ %s hook in %s was an older form (changed %s); upgraded."
	hookOldFormLine    = "The AMQ %s hook in %s is an older form (changed %s). Run `amq-remote doctor --fix`."
	upgradeFailedLine  = " The upgrade failed: %v"
	hookUnreadableLine = "Cannot read %s to check the AMQ %s hook: %v. Run `amq-remote doctor`."
	approvalLostLine   = "Approvals of this session stay in the terminal: the AMQ approval hook left %s at %s. Run `amq-remote doctor --fix`."
	restoreFailedLine  = " The restore failed: %v"
	stopLostLine       = "Turns of this session sent from Buzz do not complete: the AMQ Stop hook left %s at %s. Run `amq-remote doctor --fix`."
)

// checkHooks handles one settings file that changed: the approval hook for
// every session that reads it, the Stop hook for its native sessions.
func (s *Server) checkHooks(path string, info os.FileInfo, readers []hookReader) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	changed := "an unknown time"
	if info != nil {
		changed = info.ModTime().UTC().Format(time.RFC3339)
	}
	type hook struct {
		kind, name, lost string
		state            func(string) (string, error)
		form             func(home, settings string) (string, error)
		nativeOnly       bool
	}
	restoredAny := false
	for _, h := range []hook{
		{claude.HookApproval, "approval", approvalLostLine, claude.PermissionHookState, claude.PermissionHookForm, false},
		{claude.HookStop, "Stop", stopLostLine, claude.StopHookStateAt, claude.StopHookForm, true},
	} {
		var channels []string
		for _, r := range readers {
			if !h.nativeOnly || r.native {
				channels = append(channels, r.channel)
			}
		}
		if len(channels) == 0 {
			continue
		}
		post := func(text string) {
			for _, channel := range channels {
				_, _ = postAnswer(channel, mirror.Unmention(text), s.cfg.PostTimeout)
			}
		}
		state, err := h.state(path)
		if err != nil {
			if s.warnOnce(path, h.kind, info) {
				post(fmt.Sprintf(hookUnreadableLine, path, h.name, err))
			}
			continue
		}
		if state != claude.HookMissing {
			// A hook of an older form is upgraded as doctor --fix does it,
			// while the owner's opt-in exists; else it is reported once.
			if form, _ := h.form(home, path); !claude.FormFlagged(form) {
				continue
			}
			upgraded := false
			bin, err := remoteExecutable()
			if err == nil {
				upgraded, err = claude.UpgradeHook(home, path, h.kind, bin)
				if errors.Is(err, claude.ErrSettingsBusy) {
					delete(s.machine.settings, path)
					return
				}
			}
			if err == nil {
				if upgraded {
					restoredAny = true
					post(fmt.Sprintf(hookUpgradedLine, h.name, path, changed))
				}
				continue
			}
			if !s.warnOnce(path, h.kind, info) {
				continue
			}
			line := fmt.Sprintf(hookOldFormLine, h.name, path, changed)
			if !errors.Is(err, claude.ErrNoOptIn) {
				line += fmt.Sprintf(upgradeFailedLine, err)
			}
			post(line)
			continue
		}
		restored := false
		// With no installed amq-remote the hook is not written: a hook that
		// runs a missing binary does nothing, and the warning says why.
		bin, err := remoteExecutable()
		if err == nil {
			restored, err = claude.RestoreHook(home, path, h.kind, bin)
			if errors.Is(err, claude.ErrSettingsBusy) {
				// Another writer holds the file: check it again next tick.
				delete(s.machine.settings, path)
				return
			}
		}
		text := fmt.Sprintf(hookRestoredLine, h.name, path, changed)
		switch {
		case restored && err == nil:
			restoredAny = true
		case !s.warnOnce(path, h.kind, info):
			continue
		default:
			text = fmt.Sprintf(h.lost, path, changed)
			if err != nil {
				text += fmt.Sprintf(restoreFailedLine, err)
			}
		}
		post(text)
	}
	if restoredAny {
		// The restore or upgrade is this agent's own change: the next tick need not
		// read the file again.
		s.machine.settings[path] = settingsSeen{info: statSettings(path)}
	}
}

// warnOnce reports whether the warning that the hook of kind left the
// settings file at path is due, and records it as given: never after the
// owner removed that hook with its uninstall command, and once per version
// of the file (identity, size and mtime), kept in the state directory so a
// restart of the agent does not repeat it.
func (s *Server) warnOnce(path, kind string, info os.FileInfo) bool {
	if in, err := claude.ReadHookOptIn(path); err != nil || in.Removed(kind) {
		return false
	}
	sum := sha256.Sum256([]byte(path))
	file := filepath.Join(s.cfg.StateDir, "hook-warnings", hex.EncodeToString(sum[:12])+".json")
	warned := map[string]string{}
	if raw, err := readSmallRegular(file); err == nil {
		_ = json.Unmarshal(raw, &warned)
	}
	version := settingsVersion(info)
	if warned[kind] == version {
		return false
	}
	warned["settings"], warned[kind] = path, version
	raw, err := json.Marshal(warned)
	if err != nil {
		return false
	}
	_, _ = fsq.WriteFileAtomic(filepath.Dir(file), filepath.Base(file), append(raw, '\n'), 0o600)
	return true
}

// settingsVersion names one version of a settings file: its identity, size
// and mtime, or "none" when there is no file.
func settingsVersion(info os.FileInfo) string {
	if info == nil {
		return "none"
	}
	return fileIdentity(info) + ":" + strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}
