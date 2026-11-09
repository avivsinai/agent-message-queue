package fsq

import (
	"sync"
	"time"
)

// FileVersionSettle is how old a file's last change must be before its
// version may stand for its content. A write lands in a timestamp tick that
// can be as coarse as one second, so a second write in the tick of the first
// can leave every metadata field equal; once the last change is older than
// this, any later write has a later ctime.
const FileVersionSettle = time.Second

// FileVersion is one version of a directory entry as its metadata shows it:
// device, inode, mode, size, mtime and ctime. A missing entry is a version
// too. Equal stable versions of one path mean the entry was not written,
// replaced, renamed over, or had its mode or owner changed in between: the
// ctime is set by the kernel on each of those and cannot be set back. A
// version is a hint that lets a reader skip an unchanged file; the content
// read when it changed stays the authority.
type FileVersion struct {
	exists       bool
	dev, ino     uint64
	mode         uint32
	size         int64
	mtime, ctime int64
}

// VersionCache holds values decoded from files, each reused while its file
// keeps the stable version it had before it was read. Only successful reads
// are kept, and a path whose version is not stable is always read again. A
// cache that reaches its bound starts over, so it never grows past it.
type VersionCache[T any] struct {
	mu  sync.Mutex
	max int
	m   map[string]versioned[T]
}

type versioned[T any] struct {
	v   FileVersion
	val T
}

// NewVersionCache returns a cache of at most max paths.
func NewVersionCache[T any](max int) *VersionCache[T] {
	return &VersionCache[T]{max: max, m: map[string]versioned[T]{}}
}

// Load returns path's cached value while its version is stable and
// unchanged, else the result of read, kept when read succeeds. The version is
// taken before read, so a write during read leaves a version that differs
// next time.
func (c *VersionCache[T]) Load(path string, read func() (T, error)) (T, error) {
	v, stable, err := LstatVersion(path, time.Now())
	stable = stable && err == nil
	c.mu.Lock()
	hit, ok := c.m[path]
	c.mu.Unlock()
	if stable && ok && hit.v == v {
		return hit.val, nil
	}
	val, err := read()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || !stable {
		delete(c.m, path)
		return val, err
	}
	if len(c.m) >= c.max {
		clear(c.m)
	}
	c.m[path] = versioned[T]{v: v, val: val}
	return val, nil
}
