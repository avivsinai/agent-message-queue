package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/loginservice"
)

const boardUnitMarker = "written by amq-board service install for store "

func storeDigest(storeDir string) string {
	sum := sha256.Sum256([]byte(storeDir))
	return hex.EncodeToString(sum[:])
}

// boardServiceUnit binds one publisher to a store. The full digest in the
// marker distinguishes stores even when their short service names collide.
func boardServiceUnit(digest string) loginservice.Unit {
	u := boardUnitName(digest[:8])
	u.Marker += digest
	return u
}

func boardUnitName(suffix string) loginservice.Unit {
	return loginservice.Unit{
		Program: "amq-board",
		Label:   "io.github.avivsinai.amq-board." + suffix,
		Name:    "amq-board-" + suffix + ".service",
		Marker:  boardUnitMarker,
	}
}

func configureWatch(u *loginservice.Unit, dir, exe, session, notify, as, logPath string) {
	u.Dir, u.Exe, u.LogPath = dir, exe, logPath
	u.Description = "AMQ board for " + dir
	u.Args = []string{"sync", "--watch"}
	if session != "" {
		u.Args = append(u.Args, "--session", session)
	}
	if notify != "" {
		u.Args = append(u.Args, "--notify", notify, "--as", as)
	}
}

// serviceExecutable is the running binary, as os.Executable reports it. It
// is a variable because a test binary is never the binary a service runs.
var serviceExecutable = os.Executable

// boardStableExecutable is this binary's path as the service should run it
// (loginservice.StableBinary): the PATH entry when it names this binary, so
// a Homebrew upgrade does not break the service, else the resolved path with
// a warning. A temporary build is refused.
func boardStableExecutable() (exe, warning string, err error) {
	self, err := serviceExecutable()
	if err != nil {
		return "", "", err
	}
	exe, warning, err = loginservice.StableBinary(self, "amq-board")
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

func service(args []string) int {
	if len(args) == 0 || (args[0] != "install" && args[0] != "status" && args[0] != "uninstall") {
		fmt.Fprint(os.Stderr, boardServiceUsage)
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return 0
		}
		return exitUsage
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "amq-board: the login service is supported on macOS and Linux")
		return exitUsage
	}
	flags := flag.NewFlagSet("amq-board service "+args[0], flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "print the result as JSON")
	var session, notify, observer string
	if args[0] == "install" {
		flags.StringVar(&session, "session", "", "AMQ session for the service and its notices")
		flags.StringVar(&notify, "notify", "", "send new floor problems to these comma-separated handles")
		flags.StringVar(&observer, "as", "", "dedicated observer handle for floor notices")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, flags.Name()+": unexpected arguments:", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	switch args[0] {
	case "install":
		recipients, err := watchRecipients(notify, observer)
		if err != nil {
			fmt.Fprintln(os.Stderr, flags.Name()+":", err)
			return exitUsage
		}
		return runServiceInstall(session, recipients, observer, *asJSON)
	case "status":
		return runServiceStatus(*asJSON)
	default:
		return runServiceUninstall(*asJSON)
	}
}

const boardServiceUsage = `usage: amq-board service install [--session <name>] [--notify <handles> --as <observer>]
       amq-board service status [--json]
       amq-board service uninstall
`

// serviceProject is the working directory saved in the service.
func serviceProject() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

// A login service resolves its route from the project and its fixed arguments.
func loginBoardEnv(dir string) board.Env {
	environ := make([]string, 0)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "AM_") {
			environ = append(environ, entry)
		}
	}
	return newEnv(dir, execRunner{amqEnv: environ})
}

func runServiceInstall(session string, recipients []string, observer string, asJSON bool) int {
	notify := strings.Join(recipients, ",")
	dir, err := serviceProject()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	// The store must be initialized already; install never creates one.
	ctx := context.Background()
	store, env, err := resolveBoardSession(ctx, loginBoardEnv(dir), session)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	if _, err := store.Read(); err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	route, err := readBoardContext(ctx, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	if len(recipients) > 0 {
		roster, err := notifyRoster(ctx, env)
		if err == nil {
			err = board.ValidateNotifyRoster(route.Session, observer, recipients, roster)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "amq-board: %v; choose distinct handles registered in this session, or select their session with --session\n", err)
			return exitActionRequired
		}
	}
	exe, warning, err := boardStableExecutable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	u := boardServiceUnit(storeDigest(store.Dir))
	previous, err := readBoardInstallation(u)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	path, before := previous.path, previous.file
	prior, err := installedBoardInDirectory(dir, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	configureWatch(&u, dir, exe, session, notify, observer, filepath.Join(store.Dir, "watch.log"))
	content, err := u.Content()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	// The service manager owns its unit directory, which may be a link.
	if _, err := fsq.WriteFileAtomicFollowLinks(filepath.Dir(path), filepath.Base(path), content, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	if prior.file.Owned {
		if err := prior.remove(); err != nil {
			fmt.Fprintf(os.Stderr, "amq-board: could not retire %s: %v; retry amq-board service install\n", prior.path, err)
			return exitActionRequired
		}
	}
	started, err := u.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amq-board: wrote %s but could not start it: %v; retry amq-board service install\n", path, err)
		return exitActionRequired
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"installed": true, "replaced": before.Owned || prior.file.Owned, "project": route.Project, "session": route.Session, "store": store.Dir, "started": started, "path": path, "dir": dir, "executable": exe, "notify": notify, "as": observer, "warning": warning})
		return 0
	}
	if prior.file.Owned {
		fmt.Printf("Replaced %s, installed for another board store.\n", prior.path)
	}
	if before.Owned {
		fmt.Printf("Replaced the login service for store %s (working directory %s, previously %s).\n", store.Dir, dir, before.Dir)
	}
	if started {
		fmt.Printf("Installed and started %s: amq-board sync --watch also starts at each login.\n", path)
	} else {
		fmt.Printf("Wrote %s: amq-board sync --watch starts at the next login. There is no login session to start it in now.\n", path)
	}
	project := route.Project
	if project == "" {
		project = dir
	}
	name := route.Session
	if name == "" {
		name = "(base)"
	}
	if len(recipients) > 0 {
		fmt.Printf("Floor notices go to session %s of %s.\n", name, project)
	} else {
		fmt.Printf("Board route is session %s of %s. Floor notices are disabled.\n", name, project)
	}
	if warning != "" {
		fmt.Printf("Warning: %s.\n", warning)
	}
	return 0
}

// serviceState is the state of one project's login service.
type serviceState struct {
	State   string // missing, current or stale
	Reason  string
	Fix     string
	Dir     string
	Path    string
	Exe     string
	Version string
	LogPath string
	Session string
	Running bool
	Notify  string
	As      string
}

func runServiceStatus(asJSON bool) int {
	st, err := inspectBoardService(execRunner{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"state": st.State, "reason": st.Reason, "fix": st.Fix, "dir": st.Dir, "path": st.Path, "executable": st.Exe,
			"version": st.Version, "session": st.Session, "running": st.Running, "notify": st.Notify, "as": st.As,
		})
		return 0
	}
	if st.State == "missing" {
		fmt.Println("No login service for this project. Run amq-board service install.")
		return 0
	}
	fmt.Printf("Login service %s: %s (running: %v).\n", st.Path, st.State, st.Running)
	if st.Reason != "" {
		fmt.Printf("%s. Run %s.\n", st.Reason, st.Fix)
	}
	if st.Exe != "" {
		fmt.Printf("Runs %s (%s).\n", st.Exe, st.Version)
	}
	if st.Notify != "" {
		fmt.Printf("Floor notices: --as %s --notify %s.\n", st.As, st.Notify)
	}
	return 0
}

func boardBinaryVersion(runner board.Runner, dir, exe string) string {
	if !filepath.IsAbs(exe) {
		return "invalid executable path"
	}
	if _, err := os.Stat(exe); errors.Is(err, os.ErrNotExist) {
		return "binary missing"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runner.RunLimited(ctx, dir, []string{exe, "--version"}, 4096)
	if err != nil {
		return "version unavailable"
	}
	return strings.TrimSpace(string(out))
}

type boardInstallation struct {
	unit       loginservice.Unit
	path       string
	file       loginservice.Installed
	otherStore bool
}

func readBoardInstallation(u loginservice.Unit) (boardInstallation, error) {
	path, err := u.Path()
	if err != nil {
		return boardInstallation{}, err
	}
	file, err := u.Installed(path)
	return boardInstallation{unit: u, path: path, file: file}, err
}

// locateBoardService uses the canonical store when available. If the route
// or store is gone, only an installed unit naming this exact cwd qualifies.
func locateBoardService(dir string) (boardInstallation, error) {
	store, err := resolveStore(context.Background(), loginBoardEnv(dir))
	if err == nil {
		info, err := os.Lstat(store.Dir)
		if err == nil && info.IsDir() {
			current, err := readBoardInstallation(boardServiceUnit(storeDigest(store.Dir)))
			if err != nil {
				return boardInstallation{}, err
			}
			prior, err := installedBoardInDirectory(dir, current.path)
			if err != nil {
				return boardInstallation{}, err
			}
			if prior.file.Owned {
				prior.otherStore = true
				return prior, nil
			}
			return current, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return boardInstallation{}, err
		}
	}
	return installedBoardInDirectory(dir, "")
}

var boardStoreMarker = regexp.MustCompile(`(?m)^(?:# |<!-- )` + boardUnitMarker + `([0-9a-f]{64})(?: -->)?$`)

func installedBoardInDirectory(dir, exclude string) (boardInstallation, error) {
	// Limit the scan to board unit names, even when HOME contains glob characters.
	probe := boardUnitName(strings.Repeat("[0-9a-f]", 8))
	pattern, err := probe.Path()
	if err != nil {
		return boardInstallation{}, err
	}
	unitDir := filepath.Dir(pattern)
	entries, err := os.ReadDir(unitDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return boardInstallation{}, err
	}
	var found boardInstallation
	for _, entry := range entries {
		if matched, _ := filepath.Match(filepath.Base(pattern), entry.Name()); !matched || entry.IsDir() {
			continue
		}
		path := filepath.Join(unitDir, entry.Name())
		if path == exclude {
			continue
		}
		inst, err := probe.Installed(path)
		if err != nil {
			return boardInstallation{}, err
		}
		if !inst.Owned || inst.Dir != dir {
			continue
		}
		match := boardStoreMarker.FindSubmatch(inst.Raw)
		if match == nil {
			return boardInstallation{}, fmt.Errorf("invalid store identity in %s", path)
		}
		u := boardServiceUnit(string(match[1]))
		expected, err := u.Path()
		if err != nil || path != expected {
			return boardInstallation{}, fmt.Errorf("store identity does not match unit name %s", path)
		}
		if found.file.Owned {
			return boardInstallation{}, fmt.Errorf("both %s and %s name working directory %s; inspect the installed units", found.path, path, dir)
		}
		found = boardInstallation{unit: u, path: path, file: inst}
	}
	return found, nil
}

func inspectBoardService(runner board.Runner) (serviceState, error) {
	dir, err := serviceProject()
	if err != nil {
		return serviceState{}, err
	}
	installation, err := locateBoardService(dir)
	u, path, inst := installation.unit, installation.path, installation.file
	st := serviceState{Dir: dir, Path: path, State: "missing"}
	if err != nil {
		return st, err
	}
	if !inst.Owned {
		return st, nil
	}
	// The log path is launchd's StandardOutPath. It lives under the store, not
	// necessarily under this directory, so read it back from the unit itself.
	st.Dir, st.LogPath = inst.Dir, inst.LogPath

	st.Exe = inst.Exe
	st.Version = boardBinaryVersion(runner, inst.Dir, st.Exe)
	st.Running = u.Running() == nil
	// The notify handles from the unit's args.
	for i, arg := range inst.Args {
		if arg == "--session" && i+1 < len(inst.Args) {
			st.Session = inst.Args[i+1]
		}
		if arg == "--notify" && i+1 < len(inst.Args) {
			st.Notify = inst.Args[i+1]
		}
		if arg == "--as" && i+1 < len(inst.Args) {
			st.As = inst.Args[i+1]
		}
	}
	if installation.otherStore {
		st.State, st.Reason, st.Fix = "stale", "installed for another board store", "amq-board service install"
		return st, nil
	}
	exe, _, err := boardStableExecutable()
	if err != nil {
		return st, err
	}
	configureWatch(&u, st.Dir, exe, st.Session, st.Notify, st.As, st.LogPath)
	content, err := u.Content()
	if err != nil {
		return st, err
	}
	if bytes.Equal(inst.Raw, content) {
		st.State = "current"
	} else {
		st.State = "stale"
	}
	return st, nil
}

func runServiceUninstall(asJSON bool) int {
	dir, err := serviceProject()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	installation, err := locateBoardService(dir)
	path, inst := installation.path, installation.file
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitActionRequired
	}
	if !inst.Owned {
		if asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"removed": false, "path": path})
			return 0
		}
		fmt.Println("No login service for this project.")
		return 0
	}
	if err := installation.remove(); err != nil {
		fmt.Fprintf(os.Stderr, "amq-board: %v; retry amq-board service uninstall\n", err)
		return exitActionRequired
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"removed": true, "path": path})
		return 0
	}
	fmt.Printf("Stopped and removed %s.\n", path)
	return 0
}

func (i boardInstallation) remove() error {
	if err := i.unit.Unload(); err != nil {
		return fmt.Errorf("could not stop service: %w", err)
	}
	if logPath := i.file.LogPath; logPath != "" {
		// Only remove the output files in this unit's own board store.
		if !filepath.IsAbs(logPath) || filepath.Clean(logPath) != logPath || !strings.HasSuffix(logPath, "/extensions/board/watch.log") ||
			i.unit.Marker != boardUnitMarker+storeDigest(filepath.Dir(logPath)) {
			return fmt.Errorf("unexpected watch log path %q; inspect %s", logPath, i.path)
		}
		for _, suffix := range []string{"", ".1", ".lock"} {
			if err := os.Remove(logPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return i.unit.Remove(i.path)
}
