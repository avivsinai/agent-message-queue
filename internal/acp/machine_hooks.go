package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// The machine agent watches the Claude settings file of each session it
// serves, the settings.json in the config_dir its binding records. A file
// whose identity, size and mtime did not change since the last tick costs
// one stat. The agent never writes the file: only the install commands and
// doctor --fix change a user's saved harness settings. When a changed file
// has lost the AMQ approval hook (or, for a native session, the Stop hook),
// it says once per version of the file, in the session's channel, that the
// session's approvals (or native turns) now stay in the terminal; for a hook
// of an older form it says once that doctor --fix upgrades it. After the
// owner removed a hook with its uninstall command it says nothing about it.

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
		if !ok || !routable(r) || !s.serves(r, b) {
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

func statSettings(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// Lines the machine agent posts about a hook that left a settings file or
// is of an older form.
const (
	hookOldFormLine    = "The AMQ %s hook in %s is an older form (changed %s). Run `amq-remote doctor --fix`."
	hookUnreadableLine = "Cannot read %s to check the AMQ %s hook: %v. Run `amq-remote doctor`."
	approvalLostLine   = "Approvals of this session stay in the terminal: the AMQ approval hook left %s at %s. Run `amq-remote doctor --fix`."
	stopLostLine       = "Turns of this session sent from Buzz do not complete: the AMQ Stop hook left %s at %s. Run `amq-remote doctor --fix`."
)

// checkHooks reports on one settings file that changed: the approval hook
// for every session that reads it, the Stop hook for its native sessions.
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
		var text string
		state, err := h.state(path)
		switch {
		case err != nil:
			text = fmt.Sprintf(hookUnreadableLine, path, h.name, err)
		case state == claude.HookMissing:
			text = fmt.Sprintf(h.lost, path, changed)
		default:
			if form, _ := h.form(home, path); !claude.FormFlagged(form) {
				continue
			}
			text = fmt.Sprintf(hookOldFormLine, h.name, path, changed)
		}
		if !s.warnOnce(path, h.kind, info) {
			continue
		}
		for _, channel := range channels {
			_, _ = postAnswer(channel, mirror.Unmention(text), s.cfg.PostTimeout)
		}
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
	_ = s.writeState(file, append(raw, '\n'))
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
