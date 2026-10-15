package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The login service runs `amq-remote up --root <root>` when the user logs in,
// so a linked server sees this machine again after a reboot with no manual
// step (issue #1064): a launchd user agent on macOS, a systemd user unit on
// Linux. `up` keeps its single-instance lock, so a manual `up` while the
// service runs still refuses.
const (
	serviceLabel  = "io.github.avivsinai.amq-remote"
	serviceUnit   = "amq-remote.service"
	serviceMarker = "written by amq-remote service install"
)

// serviceRun runs the service manager (launchctl or systemctl). It is a
// variable so tests record the calls instead.
var serviceRun = func(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// service installs, removes, or reports the login service.
func service(args []string, stdout io.Writer, probe ...*jsonProbe) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "service needs install, uninstall or status")
	}
	sub := args[0]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	c.root = ""
	if err := fs.Parse(args[1:]); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeUnsupported, "the login service is supported on macOS and Linux")
	}
	root, err := serviceRoot(c.root, sub == "install")
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	path, err := servicePath()
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	switch sub {
	case "install":
		return serviceInstall(root, path, c.json, stdout)
	case "uninstall":
		return serviceUninstall(path, c.json, stdout)
	case "status":
		state, err := serviceState(root, path)
		if err != nil {
			return protocol.ExitActionRequired, err
		}
		loaded := state != "missing" && serviceLoaded() == nil
		if c.json {
			emitJSON(stdout, map[string]any{"state": state, "path": path, "root": root, "loaded": loaded})
			return 0, nil
		}
		switch state {
		case "missing":
			say(stdout, "No login service. Run amq-remote service install --root %s.", root)
		case "current":
			say(stdout, "Login service %s runs amq-remote up --root %s (loaded: %v).", path, root, loaded)
		case "stale":
			say(stdout, "Login service %s runs another binary or root (loaded: %v). Run amq-remote service install --root %s again.", path, loaded, root)
		}
		return 0, nil
	}
	return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "service needs install, uninstall or status, not %q", sub)
}

// serviceRoot is the root the service serves: --root, else the machine root,
// which install creates.
func serviceRoot(flagRoot string, create bool) (string, error) {
	if flagRoot == "" {
		if create {
			return ensureMachineRoot()
		}
		return machineRoot()
	}
	if !filepath.IsAbs(flagRoot) {
		return "", protocol.Refuse(protocol.CodeInvalid, "--root must be absolute")
	}
	root := filepath.Clean(flagRoot)
	if fi, err := os.Stat(root); create && (err != nil || !fi.IsDir()) {
		return "", fmt.Errorf("root %s is not a directory", root)
	}
	return root, nil
}

// servicePath is the unit file: ~/Library/LaunchAgents/<label>.plist on
// macOS, $XDG_CONFIG_HOME/systemd/user/amq-remote.service on Linux.
func servicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "systemd", "user", serviceUnit), nil
}

// serviceContent is the unit that runs this binary's `up` for root.
func serviceContent(root string) ([]byte, error) {
	exe, err := stableExecutable()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		return launchdPlist(exe, root, filepath.Join(root, stateDirName, "up.log")), nil
	}
	return systemdUnit(exe, root), nil
}

func serviceInstall(root, path string, asJSON bool, stdout io.Writer) (int, error) {
	if _, err := ownedService(path); err != nil {
		return protocol.ExitActionRequired, err
	}
	content, err := serviceContent(root)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o700); err != nil {
		return protocol.ExitActionRequired, err
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), content, 0o644); err != nil {
		return protocol.ExitActionRequired, err
	}
	if err := serviceLoad(path); err != nil {
		return protocol.ExitActionRequired, fmt.Errorf("wrote %s but could not start it: %w", path, err)
	}
	if asJSON {
		emitJSON(stdout, map[string]any{"installed": true, "path": path, "root": root})
		return 0, nil
	}
	say(stdout, "Installed %s: amq-remote up --root %s now starts at login.", path, root)
	return 0, nil
}

func serviceUninstall(path string, asJSON bool, stdout io.Writer) (int, error) {
	owned, err := ownedService(path)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if owned {
		serviceUnload()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return protocol.ExitActionRequired, err
		}
		if runtime.GOOS == "linux" {
			_ = serviceRun("systemctl", "--user", "daemon-reload")
		}
	}
	if asJSON {
		emitJSON(stdout, map[string]any{"removed": owned, "path": path})
		return 0, nil
	}
	if !owned {
		say(stdout, "No login service.")
		return 0, nil
	}
	say(stdout, "Stopped and removed %s.", path)
	return 0, nil
}

// ownedService reports whether path holds a unit this command wrote. It
// refuses a file it did not write, so install and uninstall never touch
// another program's unit.
func ownedService(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Contains(raw, []byte(serviceMarker)) {
		return false, fmt.Errorf("refusing to change %s; amq-remote service install did not write it", path)
	}
	return true, nil
}

// serviceState is "missing", "current" (the unit install would write for
// root now), or "stale" (another binary or root).
func serviceState(root, path string) (string, error) {
	owned, err := ownedService(path)
	if err != nil || !owned {
		return "missing", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	want, err := serviceContent(root)
	if err != nil {
		return "", err
	}
	if bytes.Equal(raw, want) {
		return "current", nil
	}
	return "stale", nil
}

func serviceLoad(path string) error {
	if runtime.GOOS == "darwin" {
		_ = serviceRun("launchctl", "bootout", launchdTarget())
		return serviceRun("launchctl", "bootstrap", launchdDomain(), path)
	}
	if err := serviceRun("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return serviceRun("systemctl", "--user", "enable", "--now", serviceUnit)
}

// serviceUnload stops the service; one that is not loaded is already
// stopped.
func serviceUnload() {
	if runtime.GOOS == "darwin" {
		_ = serviceRun("launchctl", "bootout", launchdTarget())
		return
	}
	_ = serviceRun("systemctl", "--user", "disable", "--now", serviceUnit)
}

func serviceLoaded() error {
	if runtime.GOOS == "darwin" {
		return serviceRun("launchctl", "print", launchdTarget())
	}
	return serviceRun("systemctl", "--user", "is-active", "--quiet", serviceUnit)
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func launchdTarget() string { return launchdDomain() + "/" + serviceLabel }

// launchdPlist is the launchd user agent. It starts at load (login) and is
// restarted only when `up` fails; `up` itself respawns serve.
func launchdPlist(exe, root, logPath string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + serviceMarker + " -->\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	plistString(&b, "\t", "Label", serviceLabel)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range []string{exe, "up", "--root", root} {
		b.WriteString("\t\t<string>")
		_ = xml.EscapeText(&b, []byte(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	plistString(&b, "\t", "StandardOutPath", logPath)
	plistString(&b, "\t", "StandardErrorPath", logPath)
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	plistString(&b, "\t\t", "PATH", servicePATH(exe))
	b.WriteString("\t</dict>\n</dict>\n</plist>\n")
	return b.Bytes()
}

func plistString(b *bytes.Buffer, indent, key, value string) {
	b.WriteString(indent + "<key>")
	_ = xml.EscapeText(b, []byte(key))
	b.WriteString("</key>\n" + indent + "<string>")
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString("</string>\n")
}

// systemdUnit is the systemd user unit. It starts with the user's session
// and is restarted only when `up` fails; its output goes to the journal.
func systemdUnit(exe, root string) []byte {
	var b strings.Builder
	b.WriteString("# " + serviceMarker + "\n")
	b.WriteString("[Unit]\nDescription=AMQ Remote endpoint for " + systemdEscape(root) + "\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=" + systemdQuote(exe) + " up --root " + systemdQuote(root) + "\n")
	b.WriteString("Environment=" + systemdQuote("PATH="+servicePATH(exe)) + "\n")
	b.WriteString("Restart=on-failure\nRestartSec=10\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// systemdQuote quotes one word for a systemd command line or assignment.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(systemdEscape(s)) + `"`
}

// systemdEscape doubles the specifier and variable characters systemd
// would expand.
func systemdEscape(s string) string {
	return strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
}

// servicePATH is the PATH the service runs with: this binary's directory
// first, then the usual system and Homebrew directories.
func servicePATH(exe string) string {
	dirs := []string{filepath.Dir(exe)}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, ":")
}

// stableExecutable is this binary's path as the service should run it: the
// PATH entry when it names this binary, so a Homebrew upgrade (which removes
// the versioned Cellar path) does not break the service; else the resolved
// path.
func stableExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	looked, err := exec.LookPath("amq-remote")
	if err != nil {
		return resolved, nil
	}
	if looked, err = filepath.Abs(looked); err != nil {
		return resolved, nil
	}
	lookedInfo, lookedErr := os.Stat(looked)
	exeInfo, exeErr := os.Stat(exe)
	if lookedErr == nil && exeErr == nil && os.SameFile(lookedInfo, exeInfo) {
		return looked, nil
	}
	return resolved, nil
}

// noteNoService advises the login service when the root has a link and no
// current service runs its endpoint, since its links go offline after a
// reboot until someone runs up again.
func noteNoService(note func(boundary, subject, detail, remedy string), root, stateDir string) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return
	}
	f, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil || len(f.Links) == 0 {
		return
	}
	path, err := servicePath()
	if err != nil {
		return
	}
	switch state, err := serviceState(root, path); {
	case err != nil:
		note("service", path, err.Error(), "remove it, then run `amq-remote service install --root "+root+"`")
	case state == "missing":
		note("service", "", "no login service restarts this root's endpoint after a reboot, so its links go offline", "run `amq-remote service install --root "+root+"`")
	case state == "stale":
		note("service", path, "the login service runs another binary or root", "run `amq-remote service install --root "+root+"` again")
	}
}
