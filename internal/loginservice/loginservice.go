// Package loginservice renders and inspects per-user login service units —
// a launchd user agent on macOS, a systemd user unit on Linux — and drives
// the service manager for them. It does not choose what runs: its callers
// build one Unit each, and amq-remote and amq-board are the two today.
package loginservice

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Unit is one per-user login service: a launchd user agent on macOS, a
// systemd user unit on Linux. It runs Exe with Args, starts at login, and is
// started again when it exits with a failure.
type Unit struct {
	Program     string   // command that owns the unit
	Label       string   // launchd label
	Name        string   // systemd unit file name, with ".service"
	Marker      string   // the comment that marks a file this program wrote
	Description string   // systemd Description
	Exe         string   // the binary the service runs
	Args        []string // the fixed arguments after Exe
	Dir         string   // working directory; empty for none
	LogPath     string   // macOS output file; empty for none
}

// Path is the unit file: ~/Library/LaunchAgents/<Label>.plist on macOS,
// $XDG_CONFIG_HOME/systemd/user/<Name> on Linux.
func (u Unit) Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", u.Label+".plist"), nil
	}
	if config == "" || !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "systemd", "user", u.Name), nil
}

// Installed is a unit file the package read.
type Installed struct {
	Owned   bool     // the file exists and the Unit's program wrote it
	Exe     string   // the binary it runs
	Args    []string // the ProgramArguments, or the ExecStart words with quotes undone
	Dir     string   // working directory from the unit
	LogPath string   // launchd output path
	Raw     []byte
}

// Installed reads the unit at path. It refuses a file the Unit's program did
// not write, so install and uninstall never touch another program's unit. A
// missing file reads as not owned.
func (u Unit) Installed(path string) (Installed, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Installed{}, nil
	}
	if err != nil {
		return Installed{}, err
	}
	if !bytes.Contains(raw, []byte(u.Marker)) {
		return Installed{}, fmt.Errorf("%s is not an %s login service; %s service install leaves it alone", path, u.Program, u.Program)
	}
	st := Installed{Owned: true, Raw: raw}
	if runtime.GOOS == "darwin" {
		st.Args, st.Dir, st.LogPath = plistSettings(raw)
	} else {
		st.Args = systemdExecArgs(raw)
		for line := range strings.SplitSeq(string(raw), "\n") {
			if value, ok := strings.CutPrefix(line, "WorkingDirectory="); ok {
				st.Dir = filepath.Clean(strings.ReplaceAll(value, "%%", "%"))
			}
		}
	}
	if len(st.Args) > 0 {
		st.Exe = st.Args[0]
	}
	return st, nil
}

// Content is the unit file the service manager loads, without creating or
// starting it. It refuses a control character in any field: one would break
// the unit file's line structure.
func (u Unit) Content() ([]byte, error) {
	return Render(runtime.GOOS, u)
}

// Render is Content for one platform, so tests can read both renderings.
func Render(platform string, u Unit) ([]byte, error) {
	for _, field := range append([]string{u.Program, u.Label, u.Name, u.Marker, u.Exe, u.Dir, u.LogPath, u.Description}, u.Args...) {
		if HasControl(field) {
			return nil, fmt.Errorf("%q contains a control character; a unit file line cannot carry it", field)
		}
	}
	if platform == "darwin" {
		return u.plist(), nil
	}
	return u.systemd(), nil
}

// Load starts the unit at path now, replacing a running one, and reports
// false when no login session can start it (macOS over SSH). Its started
// result is true only when the service's process was seen running.
func (u Unit) Load(path string) (bool, error) {
	if runtime.GOOS == "darwin" {
		if Run("launchctl", "print", u.launchdDomain()) != nil {
			return false, nil
		}
		if Run("launchctl", "bootout", u.launchdTarget()) == nil {
			for deadline := time.Now().Add(launchdStopWait); Run("launchctl", "print", u.launchdTarget()) == nil; {
				if time.Now().After(deadline) {
					return false, fmt.Errorf("the previous job %s did not stop within %s", u.launchdTarget(), launchdStopWait)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		if err := Run("launchctl", "bootstrap", u.launchdDomain(), path); err != nil {
			return false, err
		}
		if err := u.awaitRunning(); err != nil {
			_ = Run("launchctl", "kickstart", u.launchdTarget())
			if err = u.awaitRunning(); err != nil {
				return false, fmt.Errorf("launchd loaded the job but it does not run (%v); start it with: launchctl kickstart %s", err, u.launchdTarget())
			}
		}
		return true, nil
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", u.Name}, {"restart", u.Name}} {
		if err := Run("systemctl", append([]string{"--user"}, args...)...); err != nil {
			return false, err
		}
	}
	if err := u.awaitRunning(); err != nil {
		return false, fmt.Errorf("systemd started the unit but it is not active (%v); see: systemctl --user status %s", err, u.Name)
	}
	return true, nil
}

// Unload stops the service. One that is not loaded is already stopped; any
// other failure is returned.
func (u Unit) Unload() error {
	if runtime.GOOS == "darwin" {
		if Run("launchctl", "print", u.launchdTarget()) != nil {
			return nil
		}
		return Run("launchctl", "bootout", u.launchdTarget())
	}
	return Run("systemctl", "--user", "disable", "--now", u.Name)
}

// Running reports whether the service's process runs now.
func (u Unit) Running() error {
	if runtime.GOOS == "darwin" {
		out, err := RunOutput("launchctl", "print", u.launchdTarget())
		return launchdJobRunning(u.launchdTarget(), out, err)
	}
	return Run("systemctl", "--user", "is-active", "--quiet", u.Name)
}

// Remove removes the unit file, and on Linux the enable link and the reload
// that makes the manager forget both. It does not stop the service; call
// Unload first.
func (u Unit) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if runtime.GOOS == "linux" {
		// A failed disable leaves its wants link behind; the unit it names
		// is gone, so remove it too.
		wants := filepath.Join(filepath.Dir(path), "default.target.wants", u.Name)
		if fi, err := os.Lstat(wants); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			_ = os.Remove(wants)
		}
		_ = Run("systemctl", "--user", "daemon-reload")
	}
	return nil
}

// launchdStopWait bounds the wait for launchd to tear a booted-out job down
// before the new one is bootstrapped; a bootstrap during teardown fails.
const launchdStopWait = 5 * time.Second

// serviceStartWait bounds each wait for the service manager to show the
// job running after a start.
const serviceStartWait = 5 * time.Second

// StartSleep keeps service-start tests from waiting for a live manager.
var StartSleep = time.Sleep
var StartNow = time.Now

// awaitRunning waits until the service's process runs, at most
// serviceStartWait, and returns the last reason it did not.
func (u Unit) awaitRunning() error {
	deadline := StartNow().Add(serviceStartWait)
	for {
		err := u.Running()
		if err == nil {
			return nil
		}
		if !StartNow().Before(deadline) {
			return err
		}
		StartSleep(100 * time.Millisecond)
	}
}

// Run runs the service manager (launchctl or systemctl). It is a variable so
// tests record the calls instead.
var Run = func(name string, args ...string) error {
	_, err := RunOutput(name, args...)
	return err
}

// RunOutput is Run with the command's combined output, so a caller can read
// state the service manager prints (launchctl print reports a
// loaded-but-stopped job with exit 0).
var RunOutput = func(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (u Unit) launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func (u Unit) launchdTarget() string { return u.launchdDomain() + "/" + u.Label }

// launchdJobRunning decides running from `launchctl print` output. A loaded
// job prints its state and, when it has one, its pid: a loaded-but-stopped
// job (state = not running, no pid) also exits 0, so the exit code alone
// reported stopped services as running. Running requires the job state to
// be "running" AND a parsed positive pid; any other state, a pidless print,
// or missing, refused, or unreadable output is an error, never "running".
func launchdJobRunning(target, out string, printErr error) error {
	if printErr != nil {
		return printErr
	}
	state, pid := launchdStateAndPID(out)
	if state == "running" && launchdPIDPositive(pid) {
		return nil
	}
	if state == "" {
		return fmt.Errorf("launchctl print %s: no state in output", target)
	}
	return fmt.Errorf("launchctl print %s: state = %s, pid = %s", target, state, pid)
}

// launchdPIDPositive reports whether the printed pid parses to a positive
// integer.
func launchdPIDPositive(pid string) bool {
	n, err := strconv.Atoi(pid)
	return err == nil && n > 0
}

// launchdStateAndPID reads the job's "state = <x>" and "pid = <n>" lines
// from `launchctl print` output. Nested per-process states also appear,
// indented deeper; only the top-level job entries count.
func launchdStateAndPID(out string) (state, pid string) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		entry, value, ok := strings.Cut(strings.TrimPrefix(line, "\t"), " = ")
		if !ok {
			continue
		}
		switch strings.TrimSpace(entry) {
		case "state":
			if state == "" {
				state = strings.TrimSpace(value)
			}
		case "pid":
			if pid == "" {
				pid = strings.TrimSpace(value)
			}
		}
	}
	return state, pid
}

// HasControl reports whether s contains a control character, which a unit
// file line cannot carry.
func HasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// PATH is the PATH the service runs with: this binary's directory first,
// then the usual system and Homebrew directories.
func PATH(exe string) string {
	dirs := []string{filepath.Dir(exe)}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, ":")
}
