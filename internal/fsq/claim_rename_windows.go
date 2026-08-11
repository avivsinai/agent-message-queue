//go:build windows

package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileLinkInformation is the variable-length FILE_LINK_INFORMATION shape
// consumed by NtSetInformationFile. ReplaceIfExists is a Windows BOOLEAN
// (uint32), not a Go bool.
type fileLinkInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

type fileDispositionInformationEx struct {
	Flags uint32
}

const (
	fileDispositionDelete         = 0x00000001
	fileDispositionPosixSemantics = 0x00000002
)

// claimRename claims newPath by exclusively inserting curPath as a hard link,
// then removing the source name with POSIX disposition semantics. All handles
// are relative to the already-pinned os.Root, so an ambient rename of
// DeliveryRoot.Base cannot redirect the claim. Link insertion, unlike a rename
// performed through two already-open source handles, has one unambiguous
// winner because ReplaceIfExists is false.
func claimRename(root *DeliveryRoot, newPath, curPath string) error {
	newDir, err := root.root.Open(filepath.Dir(newPath))
	if err != nil {
		return fmt.Errorf("open claim source directory: %w", err)
	}
	defer func() { _ = newDir.Close() }()

	curDir, err := root.root.Open(filepath.Dir(curPath))
	if err != nil {
		return fmt.Errorf("open claim destination directory: %w", err)
	}
	defer func() { _ = curDir.Close() }()

	source, err := openClaimSource(windows.Handle(newDir.Fd()), filepath.Base(newPath))
	if err != nil {
		if claimTransitionAlreadyDone(err) {
			return os.ErrNotExist
		}
		mapped := windowsClaimError(err)
		return fmt.Errorf("open claim source %s: %w", root.displayPath(newPath), mapped)
	}
	defer func() { _ = windows.CloseHandle(source) }()

	err = linkClaimHandle(source, windows.Handle(curDir.Fd()), filepath.Base(curPath))
	if !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		if err != nil {
			if windowsClaimLinkUnsupported(err) {
				return fmt.Errorf("claim filesystem does not support exclusive hard-link claims: %w", errors.ErrUnsupported)
			}
			return fmt.Errorf("claim %s: %w", root.displayPath(newPath), windowsClaimError(err))
		}
		if err := removeClaimSource(source); err != nil && !claimTransitionAlreadyDone(err) {
			return &claimCommittedResidueError{Err: windowsClaimError(err)}
		}
		return nil
	}

	newInfo, statErr := root.root.Lstat(newPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return os.ErrNotExist
		}
		if os.IsPermission(statErr) {
			probe, probeErr := openClaimSource(windows.Handle(newDir.Fd()), filepath.Base(newPath))
			if probeErr == nil {
				_ = windows.CloseHandle(probe)
			} else if claimTransitionAlreadyDone(probeErr) {
				return os.ErrNotExist
			}
		}
		return fmt.Errorf("inspect claim source after collision: %w", statErr)
	}
	curInfo, statErr := root.root.Lstat(curPath)
	if statErr != nil {
		return fmt.Errorf("inspect claim destination after collision: %w", statErr)
	}
	if os.SameFile(newInfo, curInfo) {
		// A winner inserted curPath but has not yet removed newPath, or a prior
		// process crashed between those operations. Reconcile that residue and
		// report this caller as a loser rather than redelivering the message.
		if err := removeClaimSource(source); err != nil && !claimTransitionAlreadyDone(err) {
			return fmt.Errorf("remove duplicate claim source %s: %w", root.displayPath(newPath), windowsClaimError(err))
		}
		return os.ErrNotExist
	}
	return &ClaimCollisionError{
		NewPath: root.displayPath(newPath),
		CurPath: root.displayPath(curPath),
	}
}

func openClaimSource(directory windows.Handle, name string) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: directory,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var source windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&source,
		windows.SYNCHRONIZE|windows.DELETE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_DELETE|windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	return source, err
}

func linkClaimHandle(source, destinationDirectory windows.Handle, destinationName string) error {
	utf16Name, err := windows.UTF16FromString(destinationName)
	if err != nil {
		return err
	}
	nameLength := len(utf16Name) - 1 // exclude the terminating NUL
	headerLength := int(unsafe.Offsetof(fileLinkInformation{}.FileName))
	buffer := make([]byte, headerLength+nameLength*2)
	info := (*fileLinkInformation)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = destinationDirectory
	info.FileNameLength = uint32(nameLength * 2)
	copy(unsafe.Slice(&info.FileName[0], nameLength), utf16Name[:nameLength])

	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		source,
		&status,
		&buffer[0],
		uint32(len(buffer)),
		windows.FileLinkInformation,
	)
}

func removeClaimSource(source windows.Handle) error {
	info := fileDispositionInformationEx{
		Flags: fileDispositionDelete | fileDispositionPosixSemantics,
	}
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		source,
		&status,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		windows.FileDispositionInformationEx,
	)
}

func windowsClaimLinkUnsupported(err error) bool {
	return errors.Is(err, windows.STATUS_INVALID_DEVICE_REQUEST) ||
		errors.Is(err, windows.STATUS_NOT_SUPPORTED) ||
		errors.Is(err, windows.STATUS_NOT_SAME_DEVICE)
}

// claimTransitionAlreadyDone reports NT outcomes that prove another
// cooperating claimant has removed, or is currently removing, the source
// name. They are benign completion at cleanup sites and the ordinary loser
// contract at acquisition sites.
func claimTransitionAlreadyDone(err error) bool {
	return errors.Is(err, windows.STATUS_DELETE_PENDING) || os.IsNotExist(windowsClaimError(err))
}

func windowsClaimError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}
