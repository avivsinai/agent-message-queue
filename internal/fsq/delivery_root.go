package fsq

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const deliveryRootChangedRemedy = "re-run the command against the current root"

// DeliveryRootIdentity is an opaque physical-identity snapshot taken at the
// authorization boundary and consumed when the directory capability is opened.
type DeliveryRootIdentity struct {
	info os.FileInfo
}

// DeliveryRoot is an authorized, pinned filesystem capability for one AMQ
// tree. All delivery paths are resolved relative to the open directory rather
// than by reopening Base through the ambient filesystem namespace.
type DeliveryRoot struct {
	base       string
	root       *os.Root
	identity   os.FileInfo
	batchLease *pinnedBatchLease
	borrowed   bool

	syncDirForTest func(string) error
}

type pinnedBatchLease struct {
	active atomic.Bool
}

// DirectChildExistsError is returned when CreateDirectChildExclusive finds the
// name already present.
type DirectChildExistsError struct {
	Name string
}

func (e *DirectChildExistsError) Error() string {
	return fmt.Sprintf("direct child %q already exists", e.Name)
}

// beforeCreateDirectChildExclusiveForTest runs after VerifyBase and before
// Mkdir so tests can inject a racing creator.
var beforeCreateDirectChildExclusiveForTest func(r *DeliveryRoot, name string)

// beforePublishInitializedDirectChildForTest runs after staging is complete and
// the final name is confirmed absent, immediately before publication.
var beforePublishInitializedDirectChildForTest func(r *DeliveryRoot, name string)
var afterPublishInitializedDirectChildForTest func(r *DeliveryRoot, name string)

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
	if r.borrowed {
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

// FileInfo returns the physical directory snapshot captured when this root
// capability was opened. Callers may derive an opaque identity token from it;
// filesystem operations must still use the pinned capability.
func (r *DeliveryRoot) FileInfo() os.FileInfo {
	if r == nil {
		return nil
	}
	return r.identity
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

	return r.pinDirectChild(name, before)
}

// CreateDirectChildExclusive creates one direct, non-symlink child directory
// and fails if the name already exists. Session create uses this so a racing
// creator cannot silently open an existing session.
func (r *DeliveryRoot) CreateDirectChildExclusive(name string, perm os.FileMode) (*DeliveryRoot, error) {
	if r == nil || r.root == nil {
		return nil, fmt.Errorf("delivery root is closed")
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid direct child name %q", name)
	}
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	if beforeCreateDirectChildExclusiveForTest != nil {
		beforeCreateDirectChildExclusiveForTest(r, name)
	}
	if err := r.root.Mkdir(name, perm); err != nil {
		if os.IsExist(err) {
			return nil, &DirectChildExistsError{Name: name}
		}
		return nil, err
	}
	before, err := r.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is not a direct directory under delivery root", name)
	}
	return r.pinDirectChild(name, before)
}

// PublishInitializedDirectChildExclusive builds a direct child under a hidden
// sibling name and publishes it only after initialize succeeds. Publication
// uses the platform's no-replace primitive, so even an uncooperative racing
// creator can never be overwritten.
func (r *DeliveryRoot) PublishInitializedDirectChildExclusive(name string, perm os.FileMode, initialize func(*DeliveryRoot) error) (*DeliveryRoot, error) {
	if r == nil || r.root == nil {
		return nil, fmt.Errorf("delivery root is closed")
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid direct child name %q", name)
	}
	if initialize == nil {
		return nil, fmt.Errorf("direct child initializer is missing")
	}
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	if _, err := r.root.Lstat(name); err == nil {
		return nil, &DirectChildExistsError{Name: name}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("generate direct child staging name: %w", err)
	}
	stagingName := ".amq-session-" + hex.EncodeToString(random[:])
	if err := r.root.Mkdir(stagingName, perm); err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			_ = r.root.RemoveAll(stagingName)
		}
	}()
	stagingInfo, err := r.root.Lstat(stagingName)
	if err != nil {
		return nil, err
	}
	staging, err := r.pinDirectChild(stagingName, stagingInfo)
	if err != nil {
		return nil, err
	}
	keepStagingOpen := false
	defer func() {
		if !keepStagingOpen {
			_ = staging.Close()
		}
	}()
	if err := initialize(staging); err != nil {
		return nil, err
	}
	if err := staging.VerifyBase(); err != nil {
		return nil, err
	}
	if err := syncInitializedTree(staging, "."); err != nil {
		return nil, err
	}
	if _, err := r.root.Lstat(name); err == nil {
		return nil, &DirectChildExistsError{Name: name}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if beforePublishInitializedDirectChildForTest != nil {
		beforePublishInitializedDirectChildForTest(r, name)
	}
	if err := r.renameDirectChildNoReplace(stagingName, name); err != nil {
		if _, statErr := r.root.Lstat(name); statErr == nil {
			return nil, &DirectChildExistsError{Name: name}
		}
		return nil, err
	}
	published = true
	staging.base = filepath.Join(r.base, name)
	keepStagingOpen = true
	if afterPublishInitializedDirectChildForTest != nil {
		afterPublishInitializedDirectChildForTest(r, name)
	}
	after, err := r.root.Lstat(name)
	if err != nil {
		return staging, &CommittedDurabilityError{FinalPath: staging.Base(), Err: fmt.Errorf("inspect published direct child: %w", err)}
	}
	if !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(stagingInfo, after) {
		return staging, &CommittedDurabilityError{FinalPath: staging.Base(), Err: fmt.Errorf("published direct child %q changed identity", name)}
	}
	if err := r.SyncDir("."); err != nil {
		return staging, &CommittedDurabilityError{FinalPath: staging.Base(), Err: err}
	}
	return staging, nil
}

func syncInitializedTree(root *DeliveryRoot, name string) error {
	entries, err := root.ReadDir(name)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("initialized child contains symlink %q", filepath.Join(name, entry.Name()))
		}
		if !entry.IsDir() {
			continue
		}
		if err := syncInitializedTree(root, filepath.Join(name, entry.Name())); err != nil {
			return err
		}
	}
	return root.SyncDir(name)
}

func (r *DeliveryRoot) pinDirectChild(name string, before os.FileInfo) (*DeliveryRoot, error) {
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
		base:       filepath.Join(r.base, name),
		root:       childRoot,
		identity:   opened,
		batchLease: r.batchLease,
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
		base:       filepath.Join(r.base, name),
		root:       childRoot,
		identity:   opened,
		batchLease: r.batchLease,
	}, nil
}

// WithPinnedBatch verifies the ambient root identity once, then runs one
// finite operation entirely through the already-open directory capability.
// Renaming or replacing the lexical base after the callback starts cannot
// redirect any batch filesystem operation to another tree.
//
// The borrowed root and any direct children opened from it expire when the
// callback returns. Closing the borrowed root is a no-op; owned child roots
// remain closeable after expiry.
func (r *DeliveryRoot) WithPinnedBatch(fn func(*DeliveryRoot) error) error {
	if fn == nil {
		return fmt.Errorf("pinned delivery batch callback is nil")
	}
	if err := r.VerifyBase(); err != nil {
		return err
	}
	lease := &pinnedBatchLease{}
	lease.active.Store(true)
	defer lease.active.Store(false)
	batch := &DeliveryRoot{
		base:           r.base,
		root:           r.root,
		identity:       r.identity,
		batchLease:     lease,
		borrowed:       true,
		syncDirForTest: r.syncDirForTest,
	}
	return fn(batch)
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

// LayoutState classifies a pinned root's top-level queue layout.
type LayoutState int

const (
	// LayoutInitialized: agents/ exists as a real directory — the minimum
	// evidence that this tree is (or is becoming) an AMQ queue.
	LayoutInitialized LayoutState = iota
	// LayoutEmpty: the root directory has no entries at all.
	LayoutEmpty
	// LayoutForeign: the root has entries but no agents/ directory — it is
	// some other directory, not a queue.
	LayoutForeign
)

// ClassifyLayout inspects the pinned root without writes and reports whether
// it is an initialized queue, an empty directory, or a foreign tree. An
// agents/ entry that is not a real directory (symlink, file) is a hostile
// shape and fails closed with an error.
func (r *DeliveryRoot) ClassifyLayout() (LayoutState, error) {
	entries, err := r.ReadDir(".")
	if err != nil {
		return LayoutForeign, err
	}
	if len(entries) == 0 {
		return LayoutEmpty, nil
	}
	for _, entry := range entries {
		if entry.Name() != "agents" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return LayoutForeign, fmt.Errorf(
				"agents entry at %s is not a real directory; refusing to treat this tree as a queue root",
				r.displayPath("agents"),
			)
		}
		return LayoutInitialized, nil
	}
	return LayoutForeign, nil
}

// VerifyBase reports a lexical alias change after authorization. The open root
// remains the security boundary even if an alias changes immediately after
// this check; this verification makes a detected swap fail closed instead of
// silently delivering into the formerly named tree.
func (r *DeliveryRoot) VerifyBase() error {
	if r == nil || r.root == nil {
		return fmt.Errorf("delivery root is closed")
	}
	if r.batchLease != nil {
		if !r.batchLease.active.Load() {
			return fmt.Errorf("pinned delivery batch expired")
		}
		return nil
	}
	current, err := os.Stat(r.base)
	if err != nil {
		return fmt.Errorf(
			"delivery root changed after authorization: %s: %w; %s",
			r.base,
			err,
			deliveryRootChangedRemedy,
		)
	}
	if !os.SameFile(r.identity, current) {
		return fmt.Errorf(
			"delivery root changed after authorization: %s; %s",
			r.base,
			deliveryRootChangedRemedy,
		)
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

// WriteFileExclusive publishes one immutable root-relative file. It never
// replaces an existing final name; callers that use content-addressed names
// must treat os.ErrExist as a collision or an explicit idempotence decision.
func (r *DeliveryRoot) WriteFileExclusive(dir, filename string, data []byte, perm os.FileMode) (string, error) {
	if err := r.VerifyBase(); err != nil {
		return "", err
	}
	if err := r.root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	finalPath := filepath.Join(dir, filename)
	if err := r.writeAndSync(finalPath, data, perm); err != nil {
		return "", err
	}
	committedPath := r.displayPath(finalPath)
	if err := r.syncDir(dir); err != nil {
		return committedPath, &CommittedDurabilityError{FinalPath: committedPath, Err: err}
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

// OpenLockFile opens or creates a root-relative file for advisory locking.
// The file is opened O_CREATE on its stable name and is never replaced, so
// flock serializes on one inode. Callers must close the returned file.
func (r *DeliveryRoot) OpenLockFile(dir, filename string, perm os.FileMode) (*os.File, error) {
	if err := r.VerifyBase(); err != nil {
		return nil, err
	}
	if err := r.root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, filename)
	file, err := r.root.OpenFile(name, os.O_CREATE|os.O_RDWR, perm)
	if errors.Is(err, os.ErrNotExist) {
		if err := r.root.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		file, err = r.root.OpenFile(name, os.O_CREATE|os.O_RDWR, perm)
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("lock file %s is not a regular file", r.displayPath(name))
	}
	return file, nil
}

// SyncDir syncs a root-relative directory through the pinned capability.
func (r *DeliveryRoot) SyncDir(name string) error {
	if err := r.VerifyBase(); err != nil {
		return err
	}
	return r.syncDir(name)
}

// WithDLQEnvelopeLock runs fn while holding the durable, process-scoped lock
// for one DLQ envelope. The lock file is retained deliberately: its advisory
// handle lock is released by the kernel on close or process crash, so no stale
// O_EXCL sentinel can block a later recovery.
func (r *DeliveryRoot) WithDLQEnvelopeLock(agent, filename string, fn func(*DeliveryRoot) error) error {
	if err := ValidateHandle(agent); err != nil {
		return err
	}
	if err := ValidateMessageFilename(filename); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("DLQ envelope lock callback is nil")
	}
	return r.WithPinnedBatch(func(batch *DeliveryRoot) error {
		lockDir := filepath.Join("agents", agent, "dlq", "locks")
		if err := batch.root.MkdirAll(lockDir, 0o700); err != nil {
			return fmt.Errorf("prepare DLQ envelope lock directory: %w", err)
		}
		lockFile, err := batch.root.OpenFile(filepath.Join(lockDir, filename+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return fmt.Errorf("open DLQ envelope lock: %w", err)
		}
		defer func() { _ = lockFile.Close() }()
		return withExclusiveDLQEnvelopeLock(lockFile, func() error {
			// Waiting for the sidecar lock is outside the transaction. Recheck the
			// lexical authorization boundary immediately before the pinned work.
			if err := r.VerifyBase(); err != nil {
				return err
			}
			return fn(batch)
		})
	})
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
