//go:build darwin || linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

const (
	wakeInboxParentDirectoryName = "inbox"
	wakeInboxNewDirectoryName    = "new"
	wakeRepairInboxRelativePath  = wakeInboxParentDirectoryName + "/" + wakeInboxNewDirectoryName
)

type wakeRepairDirectoryIdentity struct {
	device uint64
	inode  uint64
}

type wakeInboxDir struct {
	agentPath string
	agentFile *os.File
	path      string
	file      *os.File
	mu        sync.RWMutex
	closed    bool
}

var newWakeInboxEventWatcher = func(inbox *wakeInboxDir) (wakeEventWatcher, error) {
	return inbox.NewWatcher()
}

func (*wakeAgentDir) isWakeRetainedAgent() {}

type wakeEventWatcher interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

var snapshotWakeRetainedFileInfo = func(
	inbox *wakeInboxDir,
	name string,
) (os.FileInfo, error) {
	return inbox.FileInfo(name)
}

type retainedWakeDirectoryAuthority struct {
	agentPath     string
	inboxPath     string
	agentIdentity wakeRepairDirectoryIdentity
	inboxIdentity wakeRepairDirectoryIdentity
}

type wakeDirectoryTransientError struct {
	Reason string
	Err    error
}

func (err *wakeDirectoryTransientError) Error() string {
	return fmt.Sprintf("wake directory temporarily unavailable: %s: %v", err.Reason, err.Err)
}

func (err *wakeDirectoryTransientError) Unwrap() error {
	return err.Err
}

func newWakeDirectoryTransientFailure(reason string, err error) error {
	return &wakeDirectoryTransientError{Reason: reason, Err: err}
}

func classifyWakeDirectoryOpenFailure(reason string, err error) error {
	// An open failure, including ENOENT, ENOTDIR, ELOOP, EACCES, or EMFILE,
	// cannot compare the canonical path with the retained descriptor and
	// therefore cannot prove ownership loss. A permanently unavailable lock or
	// agent directory retries indefinitely, with bounded diagnostics, until it
	// recovers or a later successful open proves an identity change.
	return newWakeDirectoryTransientFailure(reason, err)
}

func wakeRepairDirectoryIdentityForFile(file *os.File) (wakeRepairDirectoryIdentity, error) {
	if file == nil {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("wake repair directory descriptor is missing")
	}
	info, err := file.Stat()
	if err != nil {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("stat wake repair directory descriptor: %w", err)
	}
	if !info.IsDir() {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("wake repair directory descriptor is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("wake repair directory identity is unavailable")
	}
	identity := wakeRepairDirectoryIdentity{
		device: uint64(stat.Dev),
		inode:  uint64(stat.Ino),
	}
	if identity.device == 0 || identity.inode == 0 {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("wake repair directory identity is invalid")
	}
	return identity, nil
}

func wakeRepairDirectoryIdentityForFD(
	fd int,
	label string,
) (wakeRepairDirectoryIdentity, error) {
	if fd < 0 {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("%s descriptor is invalid", label)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("stat %s descriptor: %w", label, err)
	}
	identity := wakeRepairDirectoryIdentity{
		device: uint64(stat.Dev),
		inode:  uint64(stat.Ino),
	}
	if identity.device == 0 || identity.inode == 0 {
		return wakeRepairDirectoryIdentity{}, fmt.Errorf("%s identity is invalid", label)
	}
	return identity, nil
}

func newRetainedWakeDirectoryAuthority(
	agentFD, inboxFD int,
	agentPath, inboxPath string,
) (retainedWakeDirectoryAuthority, error) {
	agentIdentity, err := wakeRepairDirectoryIdentityForFD(
		agentFD,
		"retained wake agent directory",
	)
	if err != nil {
		return retainedWakeDirectoryAuthority{}, err
	}
	inboxIdentity, err := wakeRepairDirectoryIdentityForFD(
		inboxFD,
		"retained wake inbox directory",
	)
	if err != nil {
		return retainedWakeDirectoryAuthority{}, err
	}
	return retainedWakeDirectoryAuthority{
		agentPath:     agentPath,
		inboxPath:     inboxPath,
		agentIdentity: agentIdentity,
		inboxIdentity: inboxIdentity,
	}, nil
}

func (authority retainedWakeDirectoryAuthority) validateCanonical() error {
	agentFD, err := unix.Open(
		authority.agentPath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return classifyWakeDirectoryOpenFailure(
			"canonical wake repair agent directory no longer matches retained authority",
			err,
		)
	}
	defer func() { _ = unix.Close(agentFD) }()
	agentIdentity, err := wakeRepairDirectoryIdentityForFD(
		agentFD,
		"canonical wake repair agent directory",
	)
	if err != nil {
		return newWakeDirectoryTransientFailure(
			"inspect canonical wake repair agent directory",
			err,
		)
	}
	if agentIdentity != authority.agentIdentity {
		return newWakeOwnershipLoss("canonical wake repair agent directory no longer matches retained authority")
	}

	inboxFile, err := openWakeInboxNewDirectoryAt(
		agentFD,
		authority.agentPath,
		"canonical wake repair",
	)
	if err != nil {
		return err
	}
	defer func() { _ = inboxFile.Close() }()
	inboxIdentity, err := wakeRepairDirectoryIdentityForFile(inboxFile)
	if err != nil {
		return newWakeDirectoryTransientFailure(
			"inspect canonical wake repair inbox directory",
			err,
		)
	}
	if inboxIdentity != authority.inboxIdentity {
		return newWakeOwnershipLoss("canonical wake repair inbox directory no longer matches retained authority")
	}
	return nil
}

func validateCanonicalWakeAgentDir(agentDir *wakeAgentDir) error {
	if agentDir == nil {
		return newWakeOwnershipLoss("retained wake agent directory capability is missing")
	}
	var retainedIdentity wakeRepairDirectoryIdentity
	if err := agentDir.withFD(func(agentFD int) error {
		var err error
		retainedIdentity, err = wakeRepairDirectoryIdentityForFD(
			agentFD,
			"retained wake agent directory",
		)
		return err
	}); err != nil {
		return newWakeDirectoryTransientFailure(
			"inspect retained wake agent directory",
			err,
		)
	}

	openCanonical := func() (*os.File, os.FileInfo, error) {
		fd, err := unix.Open(
			agentDir.path,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return nil, nil, classifyWakeDirectoryOpenFailure(
				"canonical wake agent directory no longer matches retained authority",
				err,
			)
		}
		file := os.NewFile(uintptr(fd), agentDir.path)
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, nil, newWakeDirectoryTransientFailure(
				"stat canonical wake agent directory",
				err,
			)
		}
		if err := validateWakeAgentDir(agentDir.path, info); err != nil {
			_ = file.Close()
			return nil, nil, newWakeOwnershipLoss(err.Error())
		}
		return file, info, nil
	}

	canonical, canonicalInfo, err := openCanonical()
	if err != nil {
		return err
	}
	defer func() { _ = canonical.Close() }()
	canonicalIdentity, err := wakeRepairDirectoryIdentityForFile(canonical)
	if err != nil {
		return newWakeDirectoryTransientFailure(
			"inspect canonical wake agent directory identity",
			err,
		)
	}
	if canonicalIdentity != retainedIdentity {
		return newWakeOwnershipLoss("canonical wake agent directory no longer matches retained authority")
	}

	verification, verificationInfo, err := openCanonical()
	if err != nil {
		return err
	}
	defer func() { _ = verification.Close() }()
	if !os.SameFile(canonicalInfo, verificationInfo) {
		return newWakeOwnershipLoss("canonical wake agent directory changed while validating retained authority")
	}
	return nil
}

func validateCanonicalWakeRepairDirectories(
	root, me string,
	source wakeRepairHandoffSource,
) error {
	if canonicalWakeRoot(source.Root()) != canonicalWakeRoot(root) || source.Agent() != me {
		return fmt.Errorf("wake repair source namespace scope mismatch")
	}
	agentPath := fsq.AgentBase(root, me)
	agentFD, err := unix.Open(
		agentPath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("open canonical wake repair agent directory: %w", err)
	}
	agentFile := os.NewFile(uintptr(agentFD), agentPath)
	defer func() { _ = agentFile.Close() }()
	agentIdentity, err := wakeRepairDirectoryIdentityForFile(agentFile)
	if err != nil {
		return err
	}
	if agentIdentity.device != source.agentDirDevice ||
		agentIdentity.inode != source.agentDirInode {
		return fmt.Errorf("canonical wake repair agent directory no longer matches retained authority")
	}

	inboxFile, err := openWakeInboxNewDirectoryAt(
		agentFD,
		agentPath,
		"canonical wake repair",
	)
	if err != nil {
		return fmt.Errorf("open canonical wake repair inbox directory: %w", err)
	}
	defer func() { _ = inboxFile.Close() }()
	inboxIdentity, err := wakeRepairDirectoryIdentityForFile(inboxFile)
	if err != nil {
		return err
	}
	if inboxIdentity.device != source.inboxDirDevice ||
		inboxIdentity.inode != source.inboxDirInode {
		return fmt.Errorf("canonical wake repair inbox directory no longer matches retained authority")
	}
	return nil
}

func openValidatedWakeDirectoryAt(
	parentFD int,
	name string,
	path string,
	label string,
) (*os.File, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("%s name %q must identify one direct child", label, name)
	}
	open := func() (*os.File, os.FileInfo, error) {
		fd, err := unix.Openat(
			parentFD,
			name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return nil, nil, classifyWakeDirectoryOpenFailure(
				"open "+label,
				err,
			)
		}
		file := os.NewFile(uintptr(fd), path)
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, nil, newWakeDirectoryTransientFailure(
				"stat "+label,
				err,
			)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = file.Close()
			return nil, nil, newWakeOwnershipLoss(
				fmt.Sprintf("%s must be a directory, not a symlink", label),
			)
		}
		if err := validateWakeTargetPathOwnership(label, path, info); err != nil {
			_ = file.Close()
			return nil, nil, newWakeOwnershipLoss(err.Error())
		}
		return file, info, nil
	}

	file, openedInfo, err := open()
	if err != nil {
		return nil, err
	}
	verification, verificationInfo, err := open()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	defer func() { _ = verification.Close() }()
	if !os.SameFile(openedInfo, verificationInfo) {
		_ = file.Close()
		return nil, newWakeOwnershipLoss(fmt.Sprintf("%s changed while opening", label))
	}
	return file, nil
}

func openWakeInboxNewDirectoryAt(
	agentFD int,
	agentPath string,
	labelPrefix string,
) (*os.File, error) {
	inboxParentPath := filepath.Join(agentPath, wakeInboxParentDirectoryName)
	inboxParent, err := openValidatedWakeDirectoryAt(
		agentFD,
		wakeInboxParentDirectoryName,
		inboxParentPath,
		labelPrefix+" inbox parent directory",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inboxParent.Close() }()

	inboxPath := filepath.Join(inboxParentPath, wakeInboxNewDirectoryName)
	return openValidatedWakeDirectoryAt(
		int(inboxParent.Fd()),
		wakeInboxNewDirectoryName,
		inboxPath,
		labelPrefix+" inbox directory",
	)
}

func openWakeRepairInboxDir(agentDir *wakeAgentDir) (*wakeInboxDir, error) {
	if agentDir == nil {
		return nil, fmt.Errorf("wake repair agent directory capability is missing")
	}
	var agentFile *os.File
	var file *os.File
	err := agentDir.withFD(func(dirfd int) error {
		var err error
		agentFile, err = duplicateWakeRepairDirectoryFD(
			dirfd,
			"retained wake agent directory",
		)
		if err != nil {
			return err
		}
		file, err = openWakeInboxNewDirectoryAt(
			dirfd,
			agentDir.path,
			"retained wake",
		)
		if err != nil {
			_ = agentFile.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &wakeInboxDir{
		agentPath: agentDir.path,
		agentFile: agentFile,
		path:      file.Name(),
		file:      file,
	}, nil
}

func openWatchedWakeInboxDir(
	agentDir *wakeAgentDir,
) (*wakeInboxDir, wakeEventWatcher, error) {
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		return nil, nil, err
	}
	watcher, err := newWakeInboxEventWatcher(inboxDir)
	if err != nil {
		closeErr := inboxDir.Close()
		return nil, nil, errors.Join(err, closeErr)
	}
	return inboxDir, watcher, nil
}

func duplicateWakeRepairDirectoryFD(fd int, name string) (*os.File, error) {
	duplicateFD, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, fmt.Errorf("duplicate %s: %w", name, err)
	}
	duplicate := os.NewFile(uintptr(duplicateFD), name)
	if _, err := wakeRepairDirectoryIdentityForFile(duplicate); err != nil {
		_ = duplicate.Close()
		return nil, err
	}
	return duplicate, nil
}

func duplicateWakeRepairDirectoryFile(file *os.File, name string) (*os.File, error) {
	if file == nil {
		return nil, fmt.Errorf("%s is missing", name)
	}
	return duplicateWakeRepairDirectoryFD(int(file.Fd()), name)
}

func openInheritedWakeRepairDirectories(
	agentFile *os.File,
	inboxFile *os.File,
	source wakeRepairHandoffSource,
) (*wakeAgentDir, *wakeInboxDir, error) {
	closeBoth := func() {
		_ = closeFile(agentFile)
		_ = closeFile(inboxFile)
	}
	if err := source.validate(); err != nil {
		closeBoth()
		return nil, nil, err
	}
	agentIdentity, err := wakeRepairDirectoryIdentityForFile(agentFile)
	if err != nil {
		closeBoth()
		return nil, nil, err
	}
	if agentIdentity.device != source.agentDirDevice ||
		agentIdentity.inode != source.agentDirInode {
		closeBoth()
		return nil, nil, fmt.Errorf("inherited wake repair agent directory identity mismatch")
	}
	inboxIdentity, err := wakeRepairDirectoryIdentityForFile(inboxFile)
	if err != nil {
		closeBoth()
		return nil, nil, err
	}
	if inboxIdentity.device != source.inboxDirDevice ||
		inboxIdentity.inode != source.inboxDirInode {
		closeBoth()
		return nil, nil, fmt.Errorf("inherited wake repair inbox directory identity mismatch")
	}
	watcherAgentFile, err := duplicateWakeRepairDirectoryFile(
		agentFile,
		"retained wake watcher agent directory",
	)
	if err != nil {
		closeBoth()
		return nil, nil, err
	}
	agentDir := &wakeAgentDir{
		path: filepath.Join(source.root, "agents", source.agent),
		file: agentFile,
	}
	inboxDir := &wakeInboxDir{
		agentPath: agentDir.path,
		agentFile: watcherAgentFile,
		path:      filepath.Join(agentDir.path, wakeRepairInboxRelativePath),
		file:      inboxFile,
	}
	return agentDir, inboxDir, nil
}

func (d *wakeInboxDir) withFD(fn func(int) error) error {
	if d == nil {
		return fmt.Errorf("wake inbox directory capability is missing")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return fmt.Errorf("wake inbox directory %s is closed", d.path)
	}
	return fn(int(d.file.Fd()))
}

func (d *wakeInboxDir) withWatcherFDs(fn func(agentFD, inboxFD int) error) error {
	if d == nil {
		return fmt.Errorf("wake inbox directory capability is missing")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return fmt.Errorf("wake inbox directory %s is closed", d.path)
	}
	if d.agentFile == nil || d.file == nil {
		return fmt.Errorf("retained wake watcher directory capabilities are missing")
	}
	return fn(int(d.agentFile.Fd()), int(d.file.Fd()))
}

func (d *wakeInboxDir) ReadDir() ([]os.DirEntry, error) {
	var entries []os.DirEntry
	err := d.withFD(func(dirfd int) error {
		fd, err := unix.Openat(
			dirfd,
			".",
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("reopen retained wake inbox directory: %w", err)
		}
		scan := os.NewFile(uintptr(fd), d.path)
		defer func() { _ = scan.Close() }()
		entries, err = scan.ReadDir(-1)
		return err
	})
	return entries, err
}

func (d *wakeInboxDir) ReadHeader(name string) (format.Header, error) {
	if filepath.Base(name) != name || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
		return format.Header{}, fmt.Errorf("invalid wake message filename %q", name)
	}
	var header format.Header
	err := d.withFD(func(dirfd int) error {
		fd, err := unix.Openat(
			dirfd,
			name,
			unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("wake message %s must be a regular file", file.Name())
		}
		header, err = format.ReadHeader(file)
		return err
	})
	return header, err
}

func (d *wakeInboxDir) FileInfo(name string) (os.FileInfo, error) {
	if filepath.Base(name) != name ||
		strings.HasPrefix(name, ".") ||
		!strings.HasSuffix(name, ".md") {
		return nil, fmt.Errorf("invalid wake message filename %q", name)
	}
	var info os.FileInfo
	err := d.withFD(func(dirfd int) error {
		fd, err := unix.Openat(
			dirfd,
			name,
			unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
		defer func() { _ = file.Close() }()
		info, err = file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("wake message %s must be a regular file", file.Name())
		}
		return nil
	})
	return info, err
}

func (d *wakeInboxDir) SnapshotMessageIdentities() (map[string]wakeFileIdentity, error) {
	entries, err := d.ReadDir()
	if err != nil {
		return nil, err
	}
	baseline := make(map[string]wakeFileIdentity, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() ||
			strings.HasPrefix(name, ".") ||
			!strings.HasSuffix(name, ".md") {
			continue
		}
		info, err := snapshotWakeRetainedFileInfo(d, name)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		identity, ok := captureWakeFileIdentity(info)
		if !ok {
			return nil, fmt.Errorf("capture identity for %s", name)
		}
		baseline[name] = identity
	}
	return baseline, nil
}

func (d *wakeInboxDir) CreateBaselineBarrier() (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate wake baseline barrier name: %w", err)
		}
		name := ".wake-baseline-barrier-" + hex.EncodeToString(random[:])
		var created bool
		err := d.withFD(func(dirfd int) error {
			fd, err := unix.Openat(
				dirfd,
				name,
				unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
				0o600,
			)
			if err != nil {
				if errors.Is(err, syscall.EEXIST) {
					return nil
				}
				return err
			}
			created = true
			return unix.Close(fd)
		})
		if err != nil {
			return "", fmt.Errorf("create retained wake baseline barrier: %w", err)
		}
		if created {
			return name, nil
		}
	}
	return "", fmt.Errorf("create retained wake baseline barrier: name collision limit reached")
}

func (d *wakeInboxDir) UnlinkBaselineBarrier(name string) error {
	if filepath.Base(name) != name || !strings.HasPrefix(name, ".wake-baseline-barrier-") {
		return fmt.Errorf("invalid wake baseline barrier name %q", name)
	}
	return d.withFD(func(dirfd int) error {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("unlink retained wake baseline barrier: %w", err)
		}
		return nil
	})
}

func (d *wakeInboxDir) ValidateCanonical() error {
	return d.withWatcherFDs(func(agentFD, inboxFD int) error {
		authority, err := newRetainedWakeDirectoryAuthority(
			agentFD,
			inboxFD,
			d.agentPath,
			d.path,
		)
		if err != nil {
			return newWakeDirectoryTransientFailure(
				"inspect retained wake watcher directories",
				err,
			)
		}
		return authority.validateCanonical()
	})
}

func (d *wakeInboxDir) NewWatcher() (wakeEventWatcher, error) {
	var watcher wakeEventWatcher
	err := d.withWatcherFDs(func(agentFD, inboxFD int) error {
		var err error
		watcher, err = newRetainedWakeInboxWatcher(
			agentFD,
			inboxFD,
			d.agentPath,
			d.path,
		)
		return err
	})
	return watcher, err
}

func (d *wakeInboxDir) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	closeErr := errors.Join(closeFile(d.file), closeFile(d.agentFile))
	d.file = nil
	d.agentFile = nil
	return closeErr
}

func touchWakePresenceInDir(agentDir *wakeAgentDir, me string) error {
	if agentDir == nil {
		return fmt.Errorf("wake repair agent directory capability is missing")
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		path := filepath.Join(agentDir.path, "presence.json")
		data, _, exists, err := readWakeRepairMetadataAt(
			dirfd,
			"presence.json",
			"wake presence",
			path,
			maxWakeMetadataFileBytes,
		)
		var value presence.Presence
		switch {
		case err != nil:
			return err
		case !exists:
			value = presence.New(me, "active", "", time.Now())
		default:
			if err := json.Unmarshal(data, &value); err != nil {
				return fmt.Errorf("parse wake presence: %w", err)
			}
			value.LastSeen = time.Now().UTC().Format(time.RFC3339Nano)
		}
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		return writeWakeRepairMetadataAt(
			dirfd,
			agentDir,
			"presence.json",
			"wake presence",
			append(encoded, '\n'),
			maxWakeMetadataFileBytes,
		)
	})
}

func setWakeNotifierStatusInDir(
	agentDir *wakeAgentDir,
	me, status, mode, reason string,
) error {
	if agentDir == nil {
		return fmt.Errorf("wake agent directory capability is missing")
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		path := filepath.Join(agentDir.path, "presence.json")
		data, _, exists, err := readWakeRepairMetadataAt(
			dirfd,
			"presence.json",
			"wake presence",
			path,
			maxWakeMetadataFileBytes,
		)
		var value presence.Presence
		switch {
		case err != nil:
			return err
		case !exists:
			value = presence.New(me, "active", "", time.Now())
		default:
			if err := json.Unmarshal(data, &value); err != nil {
				return fmt.Errorf("parse wake presence: %w", err)
			}
		}
		value.NotifierStatus = status
		value.NotifierMode = mode
		value.NotifierReason = reason
		value.LastSeen = time.Now().UTC().Format(time.RFC3339Nano)
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		return writeWakeRepairMetadataAt(
			dirfd,
			agentDir,
			"presence.json",
			"wake presence",
			append(encoded, '\n'),
			maxWakeMetadataFileBytes,
		)
	})
}
