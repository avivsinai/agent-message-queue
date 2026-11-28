package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	karegistry "github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/loginservice"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	legacyloginservice "github.com/avivsinai/agent-message-queue/internal/remote/loginservice"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The login service runs `amq-remote up --root <root>` when the user logs in,
// so a linked server sees this machine again after a reboot with no manual
// step: a launchd user agent on macOS, a systemd user unit on
// Linux. `up` keeps its single-instance lock, so a manual `up` while the
// service runs still refuses. The unit renderer and the manager calls live
// in internal/loginservice, which amq-board also uses; the amq-remote names
// stay in internal/remote/loginservice for its readers.

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
	path, err := legacyloginservice.Path()
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
		running := st.State != "missing" && legacyloginservice.Definition().Running() == nil
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
		root, err = binding.EnsureMachineRoot()
	case root == "":
		root, err = binding.MachineRoot()
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
	if loginservice.HasControl(canonical) {
		return "", protocol.Refuse(protocol.CodeInvalid, "root %q contains a control character; a login service cannot run it", canonical)
	}
	return canonical, nil
}

// serviceUnit is the unit that runs exe's `up` for root.
func serviceUnit(exe, root string) loginservice.Unit {
	u := legacyloginservice.Definition()
	u.Description = "AMQ Remote endpoint for " + root
	u.Exe = exe
	u.Args = []string{"up", "--root", root}
	u.LogPath = filepath.Join(root, stateDirName, "up.log")
	return u
}

func serviceInstall(root, path string, asJSON bool, stdout io.Writer) (int, error) {
	before, err := legacyloginservice.Read(path)
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
	unit := serviceUnit(exe, root)
	content, err := unit.Content()
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	// The service manager owns its unit directory, which may be a link.
	if _, err := fsq.WriteFileAtomicFollowLinks(filepath.Dir(path), filepath.Base(path), content, 0o644); err != nil {
		return protocol.ExitActionRequired, err
	}
	replaced := ""
	if before.Owned && before.Root != "" && !sameRoot(before.Root, root) {
		replaced = before.Root
	}
	started, err := unit.Load(path)
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
	st, err := legacyloginservice.Read(path)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	var stopErr error
	if st.Owned {
		stopErr = legacyloginservice.Definition().Unload()
		if err := legacyloginservice.Definition().Remove(path); err != nil {
			return protocol.ExitActionRequired, err
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
	st, err := legacyloginservice.Read(path)
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
	content, err := serviceUnit(exe, root).Content()
	if err != nil {
		return serviceStatus{}, err
	}
	if bytes.Equal(st.Raw, content) {
		return serviceStatus{State: "current", Root: st.Root}, nil
	}
	return serviceStatus{State: "stale", Root: st.Root}, nil
}

// serviceExecutable is the running binary, as os.Executable reports it. It
// is a variable because a test binary is never the binary a service runs.
var serviceExecutable = os.Executable

// stableExecutable is this binary's path as the service should run it
// (loginservice.StableBinary): the PATH entry when it names this binary, so a
// Homebrew upgrade does not break the service, else the resolved path with
// a warning. A temporary build is refused.
func stableExecutable() (exe, warning string, err error) {
	self, err := serviceExecutable()
	if err != nil {
		return "", "", err
	}
	exe, warning, err = loginservice.StableBinary(self, "amq-remote")
	if err != nil {
		return "", "", err
	}
	if loginservice.HasControl(exe) {
		return "", "", fmt.Errorf("executable path %q contains a control character; a login service cannot run it", exe)
	}
	if warning != "" {
		warning = "the service " + warning
	}
	return exe, warning, nil
}

// noteEndpointVersion notes an endpoint for root that runs another version
// than this binary: it started before an upgrade and runs the binary it
// started with.
func noteEndpointVersion(note func(boundary, subject, detail, remedy string), root, stateDir string) {
	f, running, err := ipc.EndpointFeatures(stateDir)
	if err != nil || !running || f.Version == version {
		return
	}
	runs := "amq-remote " + f.Version
	if f.Version == "" {
		runs = "an amq-remote that does not report its version"
	}
	remedy := "stop `amq-remote up` for this root and start it again"
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if path, err := legacyloginservice.Path(); err == nil {
			if canonical, err := karegistry.CanonicalRoot(root); err == nil {
				root = canonical
			}
			if st, err := inspectService(root, path); err == nil && st.State != "missing" && st.State != "other_root" {
				remedy = "run `amq-remote service install --root " + shQuote(root) + "` again; it restarts the endpoint"
			}
		}
	}
	note("service", "", "the endpoint for this root runs "+runs+"; this amq-remote is "+version+", so the endpoint does not have its changes", remedy)
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
	path, err := legacyloginservice.Path()
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
