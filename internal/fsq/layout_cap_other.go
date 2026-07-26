//go:build !darwin && !linux

package fsq

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
)

type layoutNodeKind uint8

const (
	layoutNodeOther layoutNodeKind = iota
	layoutNodeDirectory
	layoutNodeRegular
	layoutNodeSymlink
)

type layoutNodeIdentity struct {
	info os.FileInfo
}

type layoutNodeInfo struct {
	kind     layoutNodeKind
	mode     os.FileMode
	identity layoutNodeIdentity
}

type layoutDirCapability struct {
	root *os.Root
	file *os.File
}

func openLayoutRootCapability(delivery *DeliveryRoot) (*layoutDirCapability, error) {
	root, err := delivery.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	file, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return &layoutDirCapability{root: root, file: file}, nil
}

func (c *layoutDirCapability) close() {
	if c == nil {
		return
	}
	if c.file != nil {
		_ = c.file.Close()
	}
	if c.root != nil {
		_ = c.root.Close()
	}
}

func fileInfoLayoutNode(info os.FileInfo) layoutNodeInfo {
	kind := layoutNodeOther
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = layoutNodeSymlink
	case info.IsDir():
		kind = layoutNodeDirectory
	case info.Mode().IsRegular():
		kind = layoutNodeRegular
	}
	return layoutNodeInfo{kind: kind, mode: info.Mode(), identity: layoutNodeIdentity{info: info}}
}

func lstatLayoutNode(parent *layoutDirCapability, name string) (layoutNodeInfo, error) {
	info, err := parent.root.Lstat(name)
	if err != nil {
		return layoutNodeInfo{}, err
	}
	return fileInfoLayoutNode(info), nil
}

func sameLayoutNode(left, right layoutNodeInfo) bool {
	return left.identity.info != nil && right.identity.info != nil &&
		os.SameFile(left.identity.info, right.identity.info)
}

func openLayoutDirectory(parent *layoutDirCapability, name string, before layoutNodeInfo) (*layoutDirCapability, error) {
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	after := fileInfoLayoutNode(info)
	if after.kind != layoutNodeDirectory || !sameLayoutNode(before, after) {
		_ = file.Close()
		_ = root.Close()
		return nil, errLayoutIdentityChanged
	}
	return &layoutDirCapability{root: root, file: file}, nil
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
	file, err := parent.root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, MailboxPathUnreadable, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, MailboxPathUnreadable, err
	}
	after := fileInfoLayoutNode(info)
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
	entries, err := c.file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func mkdirLayoutDirectory(parent *layoutDirCapability, name string, mode os.FileMode) error {
	return parent.root.Mkdir(name, mode)
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
	return nil
}

func layoutModeSupported(_ os.FileMode) bool {
	return true
}
