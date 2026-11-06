package acp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

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
// says so once in the session's channel; with no opt-in it only says, once
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

// watchHooks checks the settings file of each served Claude session whose
// binding records its config_dir.
func (s *Server) watchHooks() {
	readers := map[string][]hookReader{}
	var order []string
	for _, t := range s.dialogTargets() {
		b, err := binding.ReadNamed(t.name)
		if err != nil || b.ConfigDir == "" || !filepath.IsAbs(b.ConfigDir) {
			continue
		}
		path := filepath.Join(filepath.Clean(b.ConfigDir), "settings.json")
		if _, seen := readers[path]; !seen {
			order = append(order, path)
		}
		readers[path] = append(readers[path], hookReader{channel: t.channel, native: !b.Mailbox()})
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

// remoteExecutable is the amq-remote a restored hook runs: the one on PATH
// when it is the same file as the one beside this amq-acp, so an upgrade
// that removes the versioned path keeps the hook working, else the one
// beside this amq-acp, else the one on PATH. Tests replace it.
var remoteExecutable = func() (string, error) {
	var sibling, looked string
	if self, err := os.Executable(); err == nil {
		sibling = filepath.Join(filepath.Dir(self), "amq-remote")
	}
	if path, err := exec.LookPath("amq-remote"); err == nil {
		looked, _ = filepath.Abs(path)
	}
	a, errA := os.Stat(sibling)
	b, errB := os.Stat(looked)
	switch {
	case errA == nil && errB == nil && os.SameFile(a, b):
		return looked, nil
	case errA == nil && a.Mode().IsRegular():
		return sibling, nil
	case errB == nil && b.Mode().IsRegular():
		return looked, nil
	}
	return "", errors.New("no amq-remote beside amq-acp or on PATH")
}

func statSettings(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// Lines the machine agent posts about a hook that left a settings file.
const (
	hookRestoredLine = "The AMQ %s hook was missing from %s (changed %s); restored."
	approvalLostLine = "Approvals of this session stay in the terminal: the AMQ approval hook left %s at %s. Run `amq-remote doctor --fix`."
	stopLostLine     = "Turns of this session sent from Buzz do not complete: the AMQ Stop hook left %s at %s. Run `amq-remote doctor --fix`."
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
		nativeOnly       bool
	}
	restoredAny := false
	for _, h := range []hook{
		{claude.HookApproval, "approval", approvalLostLine, claude.PermissionHookState, false},
		{claude.HookStop, "Stop", stopLostLine, claude.StopHookStateAt, true},
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
		if state, err := h.state(path); err != nil || state != claude.StopHookMissing {
			continue
		}
		text := fmt.Sprintf(h.lost, path, changed)
		// With no installed amq-remote the hook is not written: a hook that
		// runs a missing binary does nothing, and the warning says what to do.
		if bin, err := remoteExecutable(); err == nil {
			restored, err := claude.RestoreHook(home, path, h.kind, bin)
			if errors.Is(err, claude.ErrSettingsBusy) {
				// Another writer holds the file: check it again next tick.
				delete(s.machine.settings, path)
				return
			}
			if err == nil && restored {
				text = fmt.Sprintf(hookRestoredLine, h.name, path, changed)
				restoredAny = true
			}
		}
		for _, channel := range channels {
			_, _ = postAnswer(channel, mirror.Unmention(text), s.cfg.PostTimeout)
		}
	}
	if restoredAny {
		// The restore is this agent's own change: the next tick need not
		// read the file again.
		s.machine.settings[path] = settingsSeen{info: statSettings(path)}
	}
}
