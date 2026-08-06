//go:build darwin || linux

package fsq

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

type layoutNodeKind uint8

const (
	layoutNodeOther layoutNodeKind = iota
	layoutNodeDirectory
	layoutNodeRegular
	layoutNodeSymlink
)

type layoutNodeIdentity struct {
	dev  uint64
	ino  uint64
	kind uint32
}

type layoutNodeInfo struct {
	kind     layoutNodeKind
	mode     os.FileMode
	identity layoutNodeIdentity
}

type layoutDirCapability struct {
	file *os.File
}

func unixLayoutOpenFlags() int {
	return unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
}

func openLayoutRootCapability(root *DeliveryRoot) (*layoutDirCapability, error) {
	file, err := root.root.OpenFile(".", unixLayoutOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	return &layoutDirCapability{file: file}, nil
}

func (c *layoutDirCapability) close() {
	if c != nil && c.file != nil {
		_ = c.file.Close()
	}
}

func unixLayoutKind(mode uint32) layoutNodeKind {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return layoutNodeDirectory
	case unix.S_IFREG:
		return layoutNodeRegular
	case unix.S_IFLNK:
		return layoutNodeSymlink
	default:
		return layoutNodeOther
	}
}

func unixLayoutInfo(stat unix.Stat_t) layoutNodeInfo {
	rawMode := uint32(stat.Mode)
	return layoutNodeInfo{
		kind: unixLayoutKind(rawMode),
		mode: os.FileMode(rawMode & 0o777),
		identity: layoutNodeIdentity{
			dev:  uint64(stat.Dev),
			ino:  uint64(stat.Ino),
			kind: rawMode & unix.S_IFMT,
		},
	}
}

func lstatLayoutNode(parent *layoutDirCapability, name string) (layoutNodeInfo, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return layoutNodeInfo{}, err
	}
	return unixLayoutInfo(stat), nil
}

func sameLayoutNode(left, right layoutNodeInfo) bool {
	return left.identity == right.identity
}

func openLayoutDirectory(parent *layoutDirCapability, name string, before layoutNodeInfo) (*layoutDirCapability, error) {
	fd, err := unix.Openat(int(parent.file.Fd()), name, unixLayoutOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fs.ErrInvalid
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	after := unixLayoutInfo(stat)
	if after.kind != layoutNodeDirectory || !sameLayoutNode(before, after) {
		_ = file.Close()
		return nil, errLayoutIdentityChanged
	}
	return &layoutDirCapability{file: file}, nil
}

func readLayoutRegularFile(parent *layoutDirCapability, name string) ([]byte, MailboxPathState, error) {
	before, err := lstatLayoutNode(parent, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, MailboxPathMissing, err
		}
		return nil, MailboxPathUnreadable, err
	}
	switch before.kind {
	case layoutNodeSymlink:
		return nil, MailboxPathSymlink, fs.ErrInvalid
	case layoutNodeRegular:
	default:
		return nil, MailboxPathNonDirectory, fs.ErrInvalid
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, MailboxPathUnreadable, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, MailboxPathUnreadable, fs.ErrInvalid
	}
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, MailboxPathUnreadable, err
	}
	after := unixLayoutInfo(stat)
	if after.kind != layoutNodeRegular || !sameLayoutNode(before, after) {
		return nil, MailboxPathChangedDuringInspection, errLayoutIdentityChanged
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, MailboxPathUnreadable, err
	}
	return data, MailboxPathDirectory, nil
}

func (c *layoutDirCapability) readDirNames() ([]string, error) {
	names, err := c.file.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func mkdirLayoutDirectory(parent *layoutDirCapability, name string, mode os.FileMode) error {
	return unix.Mkdirat(int(parent.file.Fd()), name, uint32(mode.Perm()))
}

func (c *layoutDirCapability) chmod(mode os.FileMode) error {
	return c.file.Chmod(mode)
}

func (c *layoutDirCapability) mode() (os.FileMode, error) {
	info, err := c.file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Mode(), nil
}

func (c *layoutDirCapability) sync() error {
	err := c.file.Sync()
	if isSyncUnsupported(err) {
		return nil
	}
	return err
}

func layoutModeSupported(mode os.FileMode) bool {
	return mode.Perm() == 0o700
}
