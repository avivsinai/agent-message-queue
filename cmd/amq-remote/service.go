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
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	karegistry "github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The login service runs `amq-remote up --root <root>` when the user logs in,
// so a linked server sees this machine again after a reboot with no manual
// step (issue #1064): a launchd user agent on macOS, a systemd user unit on
// Linux. `up` keeps its single-instance lock, so a manual `up` while the
// service runs still refuses. The names and the unit reader live in
// internal/remote/loginservice, so amq can read the unit too.

// serviceRun runs the service manager (launchctl or systemctl). It is a
// variable so tests record the calls instead.
var serviceRun = func(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// launchdStopWait bounds the wait for launchd to tear a booted-out job down
// before the new one is bootstrapped; a bootstrap during teardown fails.
const launchdStopWait = 5 * time.Second

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
	path, err := loginservice.Path()
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if sub == "uninstall" {
		return serviceUninstall(path, c.json, stdout)
	}
	root, err := serviceRoot(c.root, sub == "install")
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	switch sub {
	case "install":
		return serviceInstall(root, path, c.json, stdout)
	case "status":
		st, err := inspectService(root, path)
		if err != nil {
			return protocol.ExitActionRequired, err
		}
		running := st.State != "missing" && serviceRunning() == nil
		if c.json {
			emitJSON(stdout, map[string]any{"state": st.State, "path": path, "root": root, "installed_root": st.Root, "running": running})
			return 0, nil
		}
		switch st.State {
		case "missing":
			say(stdout, "No login service. Run amq-remote service install --root %s.", root)
		case "current":
			say(stdout, "Login service %s runs amq-remote up --root %s (running: %v).", path, root, running)
		case "stale":
			say(stdout, "Login service %s runs another amq-remote binary for this root (running: %v). Run amq-remote service install --root %s again.", path, running, root)
		case "other_root":
			say(stdout, "Login service %s is installed for root %s, not %s (running: %v). There is one login service per user; install it for the root whose links must stay up.", path, st.Root, root, running)
		}
		return 0, nil
	}
	return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "service needs install, uninstall or status, not %q", sub)
}

// serviceRoot is the canonical root the service serves: --root, else the
// machine root, which install creates. up canonicalizes its root the same
// way, so one root has one spelling in the unit.
func serviceRoot(flagRoot string, create bool) (string, error) {
	root := flagRoot
	var err error
	switch {
	case root == "" && create:
		root, err = ensureMachineRoot()
	case root == "":
		root, err = machineRoot()
	case !filepath.IsAbs(root):
		return "", protocol.Refuse(protocol.CodeInvalid, "--root must be absolute")
	}
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(root); create && (err != nil || !fi.IsDir()) {
		return "", fmt.Errorf("root %s is not a directory", root)
	}
	canonical, err := karegistry.CanonicalRoot(root)
	if err != nil {
		return "", err
	}
	if hasControl(canonical) {
		return "", protocol.Refuse(protocol.CodeInvalid, "root %q contains a control character; a login service cannot run it", canonical)
	}
	return canonical, nil
}

// serviceContent is the unit that runs exe's `up` for root.
func serviceContent(exe, root string) []byte {
	if runtime.GOOS == "darwin" {
		return launchdPlist(exe, root, filepath.Join(root, stateDirName, "up.log"))
	}
	return systemdUnit(exe, root)
}

func serviceInstall(root, path string, asJSON bool, stdout io.Writer) (int, error) {
	before, err := loginservice.Read(path)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	exe, warning, err := stableExecutable()
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o700); err != nil {
		return protocol.ExitActionRequired, err
	}
	if _, err := fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), serviceContent(exe, root), 0o644); err != nil {
		return protocol.ExitActionRequired, err
	}
	replaced := ""
	if before.Owned && before.Root != "" && !sameRoot(before.Root, root) {
		replaced = before.Root
	}
	started, err := serviceLoad(path)
	if err != nil {
		return protocol.ExitActionRequired, fmt.Errorf("wrote %s but could not start it: %w", path, err)
	}
	if asJSON {
		out := map[string]any{"installed": true, "started": started, "path": path, "root": root, "executable": exe}
		if replaced != "" {
			out["replaced_root"] = replaced
		}
		if warning != "" {
			out["warning"] = warning
		}
		emitJSON(stdout, out)
		return 0, nil
	}
	if replaced != "" {
		say(stdout, "Replaced the login service for root %s; there is one per user.", replaced)
	}
	if started {
		say(stdout, "Installed and started %s: amq-remote up --root %s also starts at each login.", path, root)
	} else {
		say(stdout, "Wrote %s: amq-remote up --root %s starts at the next login. There is no login session to start it in now.", path, root)
	}
	if warning != "" {
		say(stdout, "Warning: %s.", warning)
	}
	return 0, nil
}

func serviceUninstall(path string, asJSON bool, stdout io.Writer) (int, error) {
	st, err := loginservice.Read(path)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	var stopErr error
	if st.Owned {
		stopErr = serviceUnload()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return protocol.ExitActionRequired, err
		}
		if runtime.GOOS == "linux" {
			// A failed disable leaves its wants link behind; the unit it names
			// is gone, so remove it too.
			wants := filepath.Join(filepath.Dir(path), "default.target.wants", loginservice.Unit)
			if fi, err := os.Lstat(wants); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(wants)
			}
			_ = serviceRun("systemctl", "--user", "daemon-reload")
		}
	}
	if stopErr != nil {
		return protocol.ExitActionRequired, fmt.Errorf("removed %s, but could not stop the running service: %w; it stops when you log out", path, stopErr)
	}
	if asJSON {
		emitJSON(stdout, map[string]any{"removed": st.Owned, "path": path})
		return 0, nil
	}
	if !st.Owned {
		say(stdout, "No login service.")
		return 0, nil
	}
	say(stdout, "Stopped and removed %s.", path)
	return 0, nil
}

// serviceStatus is the state of the login service for a root.
type serviceStatus struct {
	State string // missing, current, stale (another binary), or other_root
	Root  string // the root the installed unit runs, when it names one
}

// inspectService compares the unit at path with the one install would write
// for root now.
func inspectService(root, path string) (serviceStatus, error) {
	st, err := loginservice.Read(path)
	if err != nil || !st.Owned {
		return serviceStatus{State: "missing"}, err
	}
	if st.Root == "" || !sameRoot(st.Root, root) {
		return serviceStatus{State: "other_root", Root: st.Root}, nil
	}
	exe, _, err := stableExecutable()
	if err != nil {
		return serviceStatus{}, err
	}
	if bytes.Equal(st.Raw, serviceContent(exe, root)) {
		return serviceStatus{State: "current", Root: st.Root}, nil
	}
	return serviceStatus{State: "stale", Root: st.Root}, nil
}

// serviceLoad starts the unit at path now, replacing a running one, and
// reports whether it started: false when there is no login session to start
// it in (macOS over SSH), so it starts at the next login.
func serviceLoad(path string) (bool, error) {
	if runtime.GOOS == "darwin" {
		if serviceRun("launchctl", "print", launchdDomain()) != nil {
			return false, nil
		}
		if serviceRun("launchctl", "bootout", launchdTarget()) == nil {
			for deadline := time.Now().Add(launchdStopWait); serviceRun("launchctl", "print", launchdTarget()) == nil; {
				if time.Now().After(deadline) {
					return false, fmt.Errorf("the previous job %s did not stop within %s", launchdTarget(), launchdStopWait)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		return true, serviceRun("launchctl", "bootstrap", launchdDomain(), path)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", loginservice.Unit}, {"restart", loginservice.Unit}} {
		if err := serviceRun("systemctl", append([]string{"--user"}, args...)...); err != nil {
			return false, err
		}
	}
	return true, nil
}

// serviceUnload stops the service. One that is not loaded is already
// stopped; any other failure is returned.
func serviceUnload() error {
	if runtime.GOOS == "darwin" {
		if serviceRun("launchctl", "print", launchdTarget()) != nil {
			return nil
		}
		return serviceRun("launchctl", "bootout", launchdTarget())
	}
	return serviceRun("systemctl", "--user", "disable", "--now", loginservice.Unit)
}

// serviceRunning reports whether the service's process runs now.
func serviceRunning() error {
	if runtime.GOOS == "darwin" {
		return serviceRun("launchctl", "print", launchdTarget())
	}
	return serviceRun("systemctl", "--user", "is-active", "--quiet", loginservice.Unit)
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func launchdTarget() string { return launchdDomain() + "/" + loginservice.Label }

// launchdPlist is the launchd user agent. It starts at load (login) and is
// restarted only when `up` fails; `up` itself respawns serve.
func launchdPlist(exe, root, logPath string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + loginservice.Marker + " -->\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	plistString(&b, "\t", "Label", loginservice.Label)
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
	b.WriteString("# " + loginservice.Marker + "\n")
	b.WriteString("[Unit]\nDescription=AMQ Remote endpoint for " + systemdSpecifiers(root) + "\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=" + systemdQuote(exe) + " up --root " + systemdQuote(root) + "\n")
	b.WriteString("Environment=" + systemdQuoteValue("PATH="+servicePATH(exe)) + "\n")
	b.WriteString("Restart=on-failure\nRestartSec=10\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// systemdQuote quotes one word of an Exec command line, where systemd
// expands specifiers (%) and variables ($).
func systemdQuote(s string) string {
	return quoteSystemd(strings.ReplaceAll(systemdSpecifiers(s), "$", "$$"))
}

// systemdQuoteValue quotes a value of a quoted assignment such as
// Environment=, where systemd expands specifiers but not variables.
func systemdQuoteValue(s string) string { return quoteSystemd(systemdSpecifiers(s)) }

func quoteSystemd(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// systemdSpecifiers doubles %, the specifier character systemd expands in
// every unit setting.
func systemdSpecifiers(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// hasControl reports whether s contains a control character, which a unit
// file line cannot carry.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
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

// serviceExecutable is the running binary, as os.Executable reports it. It
// is a variable because a test binary is never the binary a service runs.
var serviceExecutable = os.Executable

// stableExecutable is this binary's path as the service should run it: the
// PATH entry when it names this binary, so a Homebrew upgrade (which removes
// the versioned Cellar path) does not break the service; else the resolved
// path, with a warning that the service does not run the amq-remote on PATH.
// A binary under the temporary directory (go run) is refused: it disappears.
func stableExecutable() (exe, warning string, err error) {
	self, err := serviceExecutable()
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", "", err
	}
	if hasControl(resolved) {
		return "", "", fmt.Errorf("executable path %q contains a control character; a login service cannot run it", resolved)
	}
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if rel, err := filepath.Rel(tmp, resolved); err == nil && !strings.HasPrefix(rel, "..") {
			return "", "", fmt.Errorf("%s is a temporary build that disappears; install amq-remote, then run service install with it", resolved)
		}
	}
	looked, err := exec.LookPath("amq-remote")
	if err == nil {
		looked, err = filepath.Abs(looked)
	}
	if err != nil {
		return resolved, fmt.Sprintf("the service runs %s, and no amq-remote is on PATH", resolved), nil
	}
	lookedInfo, lookedErr := os.Stat(looked)
	selfInfo, selfErr := os.Stat(self)
	if lookedErr == nil && selfErr == nil && os.SameFile(lookedInfo, selfInfo) {
		return looked, "", nil
	}
	return resolved, fmt.Sprintf("the service runs %s, not the amq-remote on PATH (%s)", resolved, looked), nil
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
	path, err := loginservice.Path()
	if err != nil {
		return
	}
	if canonical, err := karegistry.CanonicalRoot(root); err == nil {
		root = canonical
	}
	install := "run `amq-remote service install --root " + root + "`"
	switch st, err := inspectService(root, path); {
	case err != nil:
		note("service", path, err.Error(), "move it away, then "+install)
	case st.State == "missing":
		note("service", "", "no login service restarts this root's endpoint after a reboot, so its links go offline", install)
	case st.State == "stale":
		note("service", path, "the login service runs another amq-remote binary for this root", install+" again")
	case st.State == "other_root":
		note("service", path, "the login service is installed for root "+st.Root+", so this root's links go offline after a reboot; there is one login service per user", install+" if this root's links must stay up")
	}
}
