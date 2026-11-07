package fsq

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WriteStateFile atomically replaces root/rel with data. root is the trust
// anchor: it is created 0700 when missing, and links in its own path are the
// caller's choice. Every directory below root is checked without following a
// symlink and created 0700 when missing, so a planted link cannot redirect the
// write. rel must be local.
func WriteStateFile(root, rel string, data []byte, perm os.FileMode) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("state file %q is not a local path below %s", rel, root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	identity, err := SnapshotDeliveryRoot(root)
	if err != nil {
		return "", err
	}
	r, err := OpenDeliveryRoot(root, identity)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	dir, name := filepath.Split(rel)
	dir = filepath.Clean(dir)
	// The no-follow walk is the guard: WriteFileAtomic's own MkdirAll
	// follows links that stay inside the root.
	if err := r.mkdirAllNoFollow(dir); err != nil {
		return "", err
	}
	return r.WriteFileAtomic(dir, name, data, perm)
}

// mkdirAllNoFollow creates each missing component of the root-relative dir at
// 0700 and refuses any component that is a symlink or not a directory.
func (r *DeliveryRoot) mkdirAllNoFollow(dir string) error {
	if dir == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := r.root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := r.root.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = r.root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return &PathSymlinkError{Path: r.displayPath(current)}
		}
		if !info.IsDir() {
			return fmt.Errorf("state path %s exists and is not a directory", r.displayPath(current))
		}
	}
	return nil
}

// WriteFileAtomicFollowLinks writes data to a temporary file in dir and
// renames it into place. It resolves dir through the ambient namespace and
// follows every symlink in it; state below a known root uses WriteStateFile.
func WriteFileAtomicFollowLinks(dir, filename string, data []byte, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpName := fmt.Sprintf(".%s.tmp-%d", filename, time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)
	finalPath := filepath.Join(dir, filename)

	if err := writeAndSync(tmpPath, data, perm); err != nil {
		return "", err
	}
	if err := SyncDir(dir); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := replaceFile(tmpPath, finalPath); err != nil {
		return "", cleanupTemp(tmpPath, err)
	}
	if err := SyncDir(dir); err != nil {
		return "", err
	}
	return finalPath, nil
}

func writeAndSync(path string, data []byte, perm os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = writeAllAndSync(file, data); err != nil {
		return err
	}
	return nil
}

type writeSyncer interface {
	Write([]byte) (int, error)
	Sync() error
}

func writeAllAndSync(file writeSyncer, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if packageFileSyncFaultForTest != nil {
		return packageFileSyncFaultForTest()
	}
	return file.Sync()
}

func cleanupTemp(path string, primary error) error {
	if primary == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (cleanup: %v)", primary, err)
	}
	return primary
}
