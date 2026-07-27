package fsq

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// DeliveryRootIdentity is an opaque physical-identity snapshot taken at the
// authorization boundary and consumed when the directory capability is opened.
type DeliveryRootIdentity struct {
	info os.FileInfo
}

// DeliveryRoot is an authorized, pinned filesystem capability for one AMQ
// tree. All delivery paths are resolved relative to the open directory rather
// than by reopening Base through the ambient filesystem namespace.
type DeliveryRoot struct {
	base     string
	root     *os.Root
	identity os.FileInfo

	syncDirForTest func(string) error
}

// SnapshotDeliveryRoot captures the physical directory identity at the
// authorization boundary. The snapshot is intentionally opaque so callers
// cannot forge or reinterpret it.
func SnapshotDeliveryRoot(base string) (DeliveryRootIdentity, error) {
	abs, err := filepath.Abs(base)
	if err != nil {
		return DeliveryRootIdentity{}, fmt.Errorf("resolve delivery root %q: %w", base, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return DeliveryRootIdentity{}, fmt.Errorf("stat delivery root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return DeliveryRootIdentity{}, fmt.Errorf("delivery root is not a directory: %s", abs)
	}
	return DeliveryRootIdentity{info: info}, nil

}

// FileInfo returns the captured identity for comparison with an existing
// platform identity token. Filesystem operations cannot be performed through
// this value.
func (i DeliveryRootIdentity) FileInfo() os.FileInfo {
	return i.info
}

// OpenDeliveryRoot opens base once and proves the opened directory is the same
// physical object authorized by expected. Subsequent operations are pinned to
// that handle and never reopen base through the ambient namespace.
func OpenDeliveryRoot(base string, expected DeliveryRootIdentity) (*DeliveryRoot, error) {
	if expected.info == nil {
		return nil, fmt.Errorf("missing authorized delivery root identity")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve delivery root %q: %w", base, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open delivery root %s: %w", abs, err)
	}
	identity, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("stat delivery root %s: %w", abs, err)
	}
	if !identity.IsDir() {
		_ = root.Close()
		return nil, fmt.Errorf("delivery root is not a directory: %s", abs)
	}
	if !os.SameFile(expected.info, identity) {
		_ = root.Close()
		return nil, fmt.Errorf("delivery root changed between authorization and capability open: %s", abs)
	}
	return &DeliveryRoot{base: abs, root: root, identity: identity}, nil
}

func (r *DeliveryRoot) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	return r.root.Close()
}

// Base returns the authorized path for diagnostics only. Filesystem operations
// must stay relative to the pinned root.
func (r *DeliveryRoot) Base() string {
	if r == nil {
		return ""
	}
	return r.base
}

// OpenOrCreateDirectChild pins one direct, non-symlink child directory beneath
// the authorized root. The before/open/after identity checks prevent a child
// swapped during validation from redirecting later writes through a symlink.
func (r *DeliveryRoot) OpenOrCreateDirectChild(name string, perm os.FileMode) (*DeliveryRoot, error) {
	if r == nil || r.root == nil {
		return nil, fmt.Errorf("delivery root is closed")
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid direct child name %q", name)
	}
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}

	before, err := r.root.Lstat(name)
	if os.IsNotExist(err) {
		if err := r.root.Mkdir(name, perm); err != nil {
			return nil, err
		}
		before, err = r.root.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is not a direct directory under delivery root", name)
	}

	childRoot, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := childRoot.Stat(".")
	if err != nil {
		_ = childRoot.Close()
		return nil, err
	}
	after, err := r.root.Lstat(name)
	if err != nil {
		_ = childRoot.Close()
		return nil, err
	}
	if !after.IsDir() || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = childRoot.Close()
		return nil, fmt.Errorf("direct child %q changed while opening", name)
	}

	return &DeliveryRoot{
		base:     filepath.Join(r.base, name),
		root:     childRoot,
		identity: opened,
	}, nil
}

// OpenDirectChild pins one existing direct, non-symlink child directory beneath
// the authorized root without creating it.
func (r *DeliveryRoot) OpenDirectChild(name string) (*DeliveryRoot, error) {
	if r == nil || r.root == nil {
		return nil, fmt.Errorf("delivery root is closed")
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid direct child name %q", name)
	}
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	before, err := r.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is not a direct directory under delivery root", name)
	}
	childRoot, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := childRoot.Stat(".")
	if err != nil {
		_ = childRoot.Close()
		return nil, err
	}
	after, err := r.root.Lstat(name)
	if err != nil {
		_ = childRoot.Close()
		return nil, err
	}
	if !after.IsDir() || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, after) || !os.SameFile(after, opened) {
		_ = childRoot.Close()
		return nil, fmt.Errorf("direct child %q changed while opening", name)
	}
	return &DeliveryRoot{
		base:     filepath.Join(r.base, name),
		root:     childRoot,
		identity: opened,
	}, nil
}

// EnsureRootDirs creates the queue-level layout through the pinned root
// capability.
func (r *DeliveryRoot) EnsureRootDirs() error {
	if err := r.VerifyBase(); err != nil {
		return err
	}
	for _, dir := range []string{"agents", "threads", "meta"} {
		if err := r.root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// EnsureAgentDirs creates one agent's mailbox layout through the pinned root
// capability.
func (r *DeliveryRoot) EnsureAgentDirs(agent string) error {
	if err := ValidateHandle(agent); err != nil {
		return err
	}
	if err := r.VerifyBase(); err != nil {
		return err
	}
	for _, leaf := range requiredMailboxLeaves {
		if err := r.root.MkdirAll(MailboxRootRelativePath(agent, leaf), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// VerifyBase reports a lexical alias change after authorization. The open root
// remains the security boundary even if an alias changes immediately after
// this check; this verification makes a detected swap fail closed instead of
// silently delivering into the formerly named tree.
func (r *DeliveryRoot) VerifyBase() error {
	if r == nil || r.root == nil {
		return fmt.Errorf("delivery root is closed")
	}
	current, err := os.Stat(r.base)
	if err != nil {
		return fmt.Errorf("delivery root changed after authorization: %s: %w", r.base, err)
	}
	if !os.SameFile(r.identity, current) {
		return fmt.Errorf("delivery root changed after authorization: %s", r.base)
	}
	return nil
}

func (r *DeliveryRoot) displayPath(name string) string {
	return filepath.Join(r.base, name)
}

// DisplayPath returns the diagnostic path for a root-relative name. The result
// must not be used for filesystem I/O.
func (r *DeliveryRoot) DisplayPath(name string) string {
	return r.displayPath(name)
}

func (r *DeliveryRoot) dirExists(name string) bool {
	info, err := r.root.Stat(name)
	return err == nil && info.IsDir()
}

func (r *DeliveryRoot) writeAndSync(name string, data []byte, perm os.FileMode) (err error) {
	file, err := r.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			_ = r.root.Remove(name)
		}
	}()
	return writeAllAndSync(file, data)
}

func (r *DeliveryRoot) cleanupTemp(name string, primary error) error {
	if primary == nil {
		return nil
	}
	if err := r.root.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (cleanup: %v)", primary, err)
	}
	return primary
}

// WriteFileAtomic writes a root-relative file through the pinned capability.
func (r *DeliveryRoot) WriteFileAtomic(dir, filename string, data []byte, perm os.FileMode) (string, error) {
	if err := r.VerifyBase(); err != nil {
		return "", err
	}
	if err := r.root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpName := fmt.Sprintf(".%s.tmp-%d", filename, time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)
	finalPath := filepath.Join(dir, filename)
	if err := r.writeAndSync(tmpPath, data, perm); err != nil {
		return "", err
	}
	if err := r.syncDir(dir); err != nil {
		return "", r.cleanupTemp(tmpPath, err)
	}
	if err := r.root.Rename(tmpPath, finalPath); err != nil {
		return "", r.cleanupTemp(tmpPath, err)
	}
	committedPath := r.displayPath(finalPath)
	if err := r.syncDir(dir); err != nil {
		return committedPath, &CommittedDurabilityError{
			FinalPath: committedPath,
			Err:       err,
		}
	}
	return committedPath, nil
}

// ReadFile reads a root-relative file through the pinned capability.
func (r *DeliveryRoot) ReadFile(name string) ([]byte, error) {
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	return r.root.ReadFile(name)
}

// ReadDir reads a root-relative directory through the pinned capability.
func (r *DeliveryRoot) ReadDir(name string) ([]os.DirEntry, error) {
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	file, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return file.ReadDir(-1)
}

// Stat stats a root-relative path through the pinned capability.
func (r *DeliveryRoot) Stat(name string) (os.FileInfo, error) {
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	return r.root.Stat(name)
}

// Remove removes a root-relative path through the pinned capability.
func (r *DeliveryRoot) Remove(name string) error {
	if err := r.VerifyBase(); err != nil {
		return err
	}
	return r.root.Remove(name)
}

// SyncDir syncs a root-relative directory through the pinned capability.
func (r *DeliveryRoot) SyncDir(name string) error {
	if err := r.VerifyBase(); err != nil {
		return err
	}
	return r.syncDir(name)
}

// ReadRegularNoFollow reads a root-relative regular file while refusing an
// initially symlinked artifact and detecting replacement between lstat/open.
func (r *DeliveryRoot) ReadRegularNoFollow(name string) ([]byte, error) {
	file, _, err := r.OpenRegularNoFollow(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

// OpenRegularNoFollow opens a root-relative regular file through the pinned
// capability while refusing symlinks and detecting replacement during open.
// The caller must close the returned file.
func (r *DeliveryRoot) OpenRegularNoFollow(name string) (*os.File, os.FileInfo, error) {
	if err := r.VerifyBase(); err != nil {
		return nil, nil, err
	}
	before, err := r.root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRegularNoFollowFile(r.displayPath(name), before); err != nil {
		return nil, nil, err
	}
	file, err := openRegularNoFollowRoot(r.root, name)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if err := validateRegularNoFollowFile(r.displayPath(name), after); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("queue artifact changed while opening: %s", r.displayPath(name))
	}
	return file, after, nil
}
