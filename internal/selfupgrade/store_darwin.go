//go:build darwin

package selfupgrade

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"golang.org/x/sys/unix"
)

// The version store is ~/.amq/state/versions/<sha256 hex>/<binary name>.
// A long-running AMQ process runs from a copy there, never from the package
// manager's file: Homebrew deletes an old Cellar folder on upgrade, and on
// macOS a process whose image lost its name cannot be attributed by tccd,
// which then denies protected volumes to every process of the same terminal.
// Only AMQ writes the store. An entry is published once, never replaced, and
// removed only while no process of this user runs it.
const (
	storeDirName  = "versions"
	storeLockName = ".lock"
)

var storeProcessPaths = sameUserProcessPaths

// PrepareAMQHomeState makes sure ~/.amq and ~/.amq/state exist, creating a
// missing level with mode 0700, and returns ~/.amq/state. Each level is
// refused, before anything is created below it, when it is a symlink, a
// non-directory, another user's directory, or a directory that group or
// other can write.
func PrepareAMQHomeState() (string, error) {
	home, err := amqhome.EnsureDir()
	if err != nil {
		return "", fmt.Errorf("prepare AMQ state directory: %w", err)
	}
	if err := checkPrivateDir(home, 0o022); err != nil {
		return "", err
	}
	state := filepath.Join(home, "state")
	if err := os.Mkdir(state, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create AMQ state directory: %w", err)
	}
	if err := checkPrivateDir(state, 0o022); err != nil {
		return "", err
	}
	return state, nil
}

func checkPrivateDir(dir string, forbidden os.FileMode) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect AMQ state directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&forbidden != 0 ||
		stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("refuse unsafe AMQ state directory %s", dir)
	}
	return nil
}

// storeRoot is the physical path of the store, so that it compares equal to
// the path the kernel reports for an image inside it.
func storeRoot() (string, error) {
	home, err := amqhome.Dir()
	if err != nil {
		return "", err
	}
	state, err := filepath.EvalSymlinks(filepath.Join(home, "state"))
	if err != nil {
		return "", err
	}
	return filepath.Join(state, storeDirName), nil
}

func ensureStoreRoot() (string, error) {
	if _, err := PrepareAMQHomeState(); err != nil {
		return "", err
	}
	root, err := storeRoot()
	if err != nil {
		return "", fmt.Errorf("resolve version store: %w", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create version store: %w", err)
	}
	if err := checkPrivateDir(root, 0o077); err != nil {
		return "", err
	}
	return root, nil
}

// InStore reports whether path is an image inside the version store.
func InStore(path string) bool {
	root, err := storeRoot()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 2 && validStoreKey(parts[0]) && parts[1] != "" && parts[1] != ".." &&
		!strings.HasPrefix(parts[1], ".")
}

func validStoreKey(key string) bool {
	return ValidSHA256("sha256:" + key)
}

// RunFromStore re-executes the running image from its copy in the version
// store, with the same argv and environment, unless it already runs from the
// store. A long-running command calls it first, before it takes any lock or
// state. It returns only when the process keeps its current image: nil when
// that image is already in the store, an error when the store could not be
// used, which the caller reports once and then runs on.
func RunFromStore(embeddedVersion string) error {
	started, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running image: %w", err)
	}
	executable, err := filepath.EvalSymlinks(started)
	if err != nil {
		return fmt.Errorf("resolve running image: %w", err)
	}
	executable = filepath.Clean(executable)
	if InStore(executable) {
		return nil
	}
	running, err := CaptureImageEvidence(executable, embeddedVersion)
	if err != nil {
		return fmt.Errorf("capture running image: %w", err)
	}
	// The path as started keeps a package manager's stable link, which
	// InstalledImage callers resolve siblings and durable paths from.
	return ExecImage(running, os.Args, withInstalledImage(os.Environ(), started))
}

// withInstalledImage is env with EnvInstalledImage set to path.
func withInstalledImage(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, EnvInstalledImage+"=") {
			out = append(out, entry)
		}
	}
	return append(out, EnvInstalledImage+"="+path)
}

// storeImage publishes candidate into the version store, or finds the entry
// that already holds the same content, and returns the stored image's
// evidence after its code signature verifies. The returned lock stays held
// until the caller execs the entry, so no prune removes it in between; its
// descriptor closes on exec, when the process already runs from the entry.
func storeImage(candidate ImageEvidence) (ImageEvidence, *os.File, error) {
	root, err := ensureStoreRoot()
	if err != nil {
		return ImageEvidence{}, nil, err
	}
	lock, exclusive, err := lockStore(root)
	if err != nil {
		return ImageEvidence{}, nil, err
	}
	evidence, err := storeImageLocked(root, exclusive, candidate)
	if err != nil {
		_ = lock.Close()
		return ImageEvidence{}, nil, err
	}
	return evidence, lock, nil
}

// lockStore takes the store lock exclusively when no other process holds it,
// which entitles the caller to prune, and otherwise waits for a shared hold.
func lockStore(root string) (*os.File, bool, error) {
	fd, err := unix.Open(
		filepath.Join(root, storeLockName),
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, false, fmt.Errorf("open version store lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), filepath.Join(root, storeLockName))
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		return lock, true, nil
	}
	if err := unix.Flock(fd, unix.LOCK_SH); err != nil {
		_ = lock.Close()
		return nil, false, fmt.Errorf("lock version store: %w", err)
	}
	return lock, false, nil
}

func storeImageLocked(root string, exclusive bool, candidate ImageEvidence) (ImageEvidence, error) {
	source, err := openBoundImage(candidate.ExecutionPath)
	if err != nil {
		return ImageEvidence{}, err
	}
	defer func() { _ = source.Close() }()
	sourceEvidence, err := CaptureImageEvidenceFromOpenFile(
		source,
		candidate.ExecutionPath,
		candidate.EmbeddedVersion,
		ImageMethodPathnameObserved,
	)
	if err != nil {
		return ImageEvidence{}, err
	}
	if !SameDarwinStagedImageEvidence(sourceEvidence, candidate) {
		return ImageEvidence{}, errors.New("self-upgrade candidate changed while binding")
	}

	dir := filepath.Join(root, strings.TrimPrefix(candidate.SHA256, "sha256:"))
	stored := filepath.Join(dir, filepath.Base(candidate.ExecutionPath))
	// Prune before the entry is made: a prune removes a directory it empties,
	// so after it the entry this call publishes into must be made again.
	if exclusive {
		_ = pruneStore(root, stored)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return ImageEvidence{}, fmt.Errorf("create version store entry: %w", err)
	}
	if err := checkPrivateDir(dir, 0o077); err != nil {
		return ImageEvidence{}, err
	}
	if _, err := os.Lstat(stored); errors.Is(err, os.ErrNotExist) {
		if err := publishStoreImage(source, dir, stored); err != nil {
			return ImageEvidence{}, err
		}
	} else if err != nil {
		return ImageEvidence{}, fmt.Errorf("inspect version store entry: %w", err)
	}
	evidence, err := verifyStoredImage(stored, candidate)
	if err != nil {
		return ImageEvidence{}, err
	}
	if err := verifyDarwinCodeSignature(stored); err != nil {
		return ImageEvidence{}, err
	}
	return evidence, nil
}

func openBoundImage(path string) (*os.File, error) {
	lstat, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat self-upgrade candidate: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("self-upgrade candidate must not be a symlink")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open self-upgrade candidate: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open self-upgrade candidate file descriptor")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened self-upgrade candidate: %w", err)
	}
	if !os.SameFile(lstat, opened) {
		_ = file.Close()
		return nil, errors.New("self-upgrade candidate changed while binding")
	}
	return file, nil
}

// publishStoreImage copies source to a temporary file in dir, syncs it, and
// links it to stored. A link never replaces a file: when another process
// published the entry first, its file stays and is verified by the caller.
func publishStoreImage(source *os.File, dir, stored string) error {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind image for the version store: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(stored)+".tmp-")
	if err != nil {
		return fmt.Errorf("create version store temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	_, copyErr := io.Copy(temp, source)
	if copyErr == nil {
		copyErr = temp.Chmod(0o755)
	}
	if copyErr == nil {
		copyErr = temp.Sync()
	}
	if closeErr := temp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("copy image into the version store: %w", copyErr)
	}
	if err := os.Link(tempPath, stored); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("publish version store entry: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove version store temporary file: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fsync(fd)
}

// verifyStoredImage requires the stored file to hold exactly the candidate's
// content and returns its evidence as an exec-bound observation.
func verifyStoredImage(stored string, candidate ImageEvidence) (ImageEvidence, error) {
	file, err := openBoundImage(stored)
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("open version store entry: %w", err)
	}
	defer func() { _ = file.Close() }()
	evidence, err := CaptureImageEvidenceFromOpenFile(
		file,
		stored,
		candidate.EmbeddedVersion,
		ImageMethodPathnameExecObserved,
	)
	if err != nil {
		return ImageEvidence{}, fmt.Errorf("verify version store entry: %w", err)
	}
	if evidence.SHA256 != candidate.SHA256 || evidence.Size != candidate.Size {
		return ImageEvidence{}, fmt.Errorf("version store entry %s does not hold the candidate image", stored)
	}
	return evidence, nil
}

// pruneStore removes store entries that no process of this user runs. The
// caller holds the store lock exclusively, so no other process is between
// publishing or verifying an entry and running it. An entry whose user cannot
// be proven absent stays: any failure to list or inspect a process keeps
// every entry.
func pruneStore(root, keep string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type image struct{ dir, path string }
	var images []image
	names := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || !validStoreKey(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, file := range files {
			images = append(images, image{dir: dir, path: filepath.Join(dir, file.Name())})
			names[strings.TrimPrefix(strings.SplitN(file.Name(), ".tmp-", 2)[0], ".")] = true
		}
	}
	if len(images) == 0 {
		return nil
	}
	running, err := storeProcessPaths(names)
	if err != nil {
		return err
	}
	for _, candidate := range images {
		if candidate.path == keep || running[candidate.path] {
			continue
		}
		info, err := os.Lstat(candidate.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(candidate.path); err != nil {
			return err
		}
		_ = os.Remove(candidate.dir)
	}
	return nil
}

// sameUserProcessPaths returns the image path of every process of this user
// whose command name is one of names. Any other failure, including a process
// whose image has no name left (ENOENT), fails the whole listing, so a prune
// keeps every entry until that process ends.
func sameUserProcessPaths(names map[string]bool) (map[string]bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Geteuid())
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	paths := map[string]bool{}
	for _, proc := range procs {
		comm := unix.ByteSliceToString(proc.Proc.P_comm[:])
		if !names[comm] {
			continue
		}
		pid := int(proc.Proc.P_pid)
		path, err := ProcessExecutablePath(pid)
		if errors.Is(err, unix.ESRCH) {
			// The process exited, so it runs from no entry.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect process %d: %w", pid, err)
		}
		paths[path] = true
	}
	return paths, nil
}
