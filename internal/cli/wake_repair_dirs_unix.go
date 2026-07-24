//go:build darwin || linux

package cli

import (
	"encoding/json"
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

const wakeRepairInboxRelativePath = "inbox/new"

type wakeRepairDirectoryIdentity struct {
	device uint64
	inode  uint64
}

type wakeInboxDir struct {
	path   string
	file   *os.File
	mu     sync.RWMutex
	closed bool
}

type wakeEventWatcher interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
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

	inboxPath := filepath.Join(agentPath, wakeRepairInboxRelativePath)
	inboxFD, err := unix.Openat(
		agentFD,
		wakeRepairInboxRelativePath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("open canonical wake repair inbox directory: %w", err)
	}
	inboxFile := os.NewFile(uintptr(inboxFD), inboxPath)
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

func openWakeRepairInboxDir(agentDir *wakeAgentDir) (*wakeInboxDir, error) {
	if agentDir == nil {
		return nil, fmt.Errorf("wake repair agent directory capability is missing")
	}
	var file *os.File
	err := agentDir.withFD(func(dirfd int) error {
		fd, err := unix.Openat(
			dirfd,
			wakeRepairInboxRelativePath,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("open retained wake inbox directory: %w", err)
		}
		file = os.NewFile(uintptr(fd), filepath.Join(agentDir.path, wakeRepairInboxRelativePath))
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("stat retained wake inbox directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = file.Close()
			return fmt.Errorf("retained wake inbox must be a directory")
		}
		if err := validateWakeTargetPathOwnership("retained wake inbox directory", file.Name(), info); err != nil {
			_ = file.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &wakeInboxDir{path: file.Name(), file: file}, nil
}

func duplicateWakeRepairDirectoryFile(file *os.File, name string) (*os.File, error) {
	if file == nil {
		return nil, fmt.Errorf("%s is missing", name)
	}
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, fmt.Errorf("duplicate %s: %w", name, err)
	}
	duplicate := os.NewFile(uintptr(fd), name)
	if _, err := wakeRepairDirectoryIdentityForFile(duplicate); err != nil {
		_ = duplicate.Close()
		return nil, err
	}
	return duplicate, nil
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
	agentDir := &wakeAgentDir{
		path: filepath.Join(source.root, "agents", source.agent),
		file: agentFile,
	}
	inboxDir := &wakeInboxDir{
		path: filepath.Join(agentDir.path, wakeRepairInboxRelativePath),
		file: inboxFile,
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

func (d *wakeInboxDir) NewWatcher() (wakeEventWatcher, error) {
	var watcher wakeEventWatcher
	err := d.withFD(func(dirfd int) error {
		var err error
		watcher, err = newRetainedWakeInboxWatcher(dirfd, d.path)
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
	return d.file.Close()
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
