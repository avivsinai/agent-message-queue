//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

const wakeStagedInbox = ".wake-inbox-provisioning"

type wakeMailboxPathError struct {
	path string
	err  error
}

func (e *wakeMailboxPathError) Error() string {
	return fmt.Sprintf("provision wake mailbox at %s: %v; fix the named path, then start the wake again", e.path, e.err)
}

func (e *wakeMailboxPathError) Unwrap() error { return e.err }

// A present inbox belongs to the normal watcher, including when inbox/new
// is temporarily unavailable. Only proven absence authorizes provisioning.
func ensureWakeMailbox(agent *wakeAgentDir) error {
	provisioned := false
	err := agent.withFD(func(fd int) error {
		var st unix.Stat_t
		err := unix.Fstatat(fd, wakeInboxParentDirectoryName, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT):
			provisioned = true
			return provisionWakeMailbox(fd, agent.path)
		case err != nil || st.Mode&unix.S_IFMT == unix.S_IFDIR:
			return nil
		default:
			return &wakeMailboxPathError{filepath.Join(agent.path, wakeInboxParentDirectoryName), errors.New("expected a directory, not a file or symlink")}
		}
	})
	if err != nil || !provisioned {
		return err
	}
	return validateCanonicalWakeAgentDir(agent)
}

// The keys are single child names from the required mailbox layout.
// The empty map at a leaf also describes what an empty staging leaf may hold.
type wakeMailboxLayout map[string]wakeMailboxLayout

func requiredWakeMailboxLayout() wakeMailboxLayout {
	root := wakeMailboxLayout{}
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		children := root
		for _, name := range strings.Split(string(leaf), "/") {
			if children[name] == nil {
				children[name] = wakeMailboxLayout{}
			}
			children = children[name]
		}
	}
	return root
}

func provisionWakeMailbox(agentFD int, agentPath string) error {
	layout := requiredWakeMailboxLayout()
	inbox := layout[wakeInboxParentDirectoryName]
	delete(layout, wakeInboxParentDirectoryName)

	// Preflight every existing component before the first mkdir. The
	// mutating pass independently opens each child without following links.
	for _, create := range []bool{false, true} {
		for name, children := range layout {
			if err := visitWakeMailboxDir(agentFD, agentPath, name, children, create, false); err != nil {
				return err
			}
		}
		if err := visitWakeMailboxDir(agentFD, agentPath, wakeStagedInbox, inbox, create, true); err != nil {
			return err
		}
	}
	return nil
}

// Each operation names a single child of a held directory. Sync even when
// the directory already exists: an earlier start may have died before sync.
func visitWakeMailboxDir(parentFD int, parentPath, name string, children wakeMailboxLayout, create, staged bool) error {
	path := filepath.Join(parentPath, name)
	if create {
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return &wakeMailboxPathError{path, err}
		}
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		if !create && errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &wakeMailboxPathError{path, err}
	}
	dir := os.NewFile(uintptr(fd), path)
	defer func() { _ = dir.Close() }()
	if staged {
		if err := checkWakeStagingContents(dir, children); err != nil {
			return err
		}
	}
	for child, descendants := range children {
		if err := visitWakeMailboxDir(fd, path, child, descendants, create, staged); err != nil {
			return err
		}
	}
	if !create {
		return nil
	}
	if err := dir.Sync(); err != nil {
		return &wakeMailboxPathError{path, err}
	}
	if name == wakeStagedInbox {
		// Commit the siblings before publishing inbox. After a crash its
		// presence must never advertise an incomplete mailbox.
		if err := unix.Fsync(parentFD); err != nil {
			return &wakeMailboxPathError{parentPath, err}
		}
	}
	if err := verifyWakeMailboxChild(parentFD, name, fd); err != nil {
		return &wakeMailboxPathError{path, err}
	}
	if name != wakeStagedInbox {
		return nil
	}
	if err := wakeRenameNoReplaceAt(parentFD, name, parentFD, wakeInboxParentDirectoryName); err != nil {
		return &wakeMailboxPathError{filepath.Join(parentPath, wakeInboxParentDirectoryName), err}
	}
	if err := unix.Fsync(parentFD); err != nil {
		return &wakeMailboxPathError{parentPath, err}
	}
	return nil
}

// A same-name replacement is not the directory just prepared and synced.
func verifyWakeMailboxChild(parentFD int, name string, fd int) error {
	var opened, named unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if err := unix.Fstatat(parentFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if named.Dev != opened.Dev || named.Ino != opened.Ino {
		return errors.New("directory changed during provisioning")
	}
	return nil
}

func checkWakeStagingContents(dir *os.File, allowed wakeMailboxLayout) error {
	info, err := dir.Stat()
	if err != nil {
		return &wakeMailboxPathError{dir.Name(), err}
	}
	if err := validateWakeTargetPathOwnership("staged inbox directory", dir.Name(), info); err != nil {
		return &wakeMailboxPathError{dir.Name(), err}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return &wakeMailboxPathError{dir.Name(), errors.New("expected a private provisioning directory")}
	}
	names, err := dir.Readdirnames(len(allowed) + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return &wakeMailboxPathError{dir.Name(), err}
	}
	for _, name := range names {
		if _, ok := allowed[name]; !ok {
			return &wakeMailboxPathError{filepath.Join(dir.Name(), name), errors.New("unexpected entry in staged inbox")}
		}
	}
	return nil
}
