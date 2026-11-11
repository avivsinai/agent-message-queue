package requests

import (
	"os"
	"reflect"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// statIdentity names one version of a record file: the physical file plus its
// size and modification time.
type statIdentity struct {
	file  string
	size  int64
	mtime int64
}

// fileStatIdentity stats path without following a symlink. It reports false
// for anything but a regular file, so such a path is never cached and the
// bounded reader judges it.
func fileStatIdentity(path string) (statIdentity, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return statIdentity{}, false
	}
	file, err := fsq.StableFileIdentityInfo(info)
	if err != nil {
		return statIdentity{}, false
	}
	return statIdentity{file: file, size: info.Size(), mtime: info.ModTime().UnixNano()}, true
}

// recordCache keeps one decoded, normalized record per path while the file's
// identity is unchanged. It stores and returns private deep copies, so no
// caller can change a cached record.
type recordCache struct {
	mu sync.Mutex
	m  map[string]cachedRecord
}

type cachedRecord struct {
	id  statIdentity
	rec *Record
}

func (c *recordCache) get(path string, id statIdentity) *Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[path]; ok && e.id == id {
		// A cached entry was copied once already, so this copy succeeds.
		if rec, ok := deepCopy(e.rec); ok {
			return rec
		}
	}
	return nil
}

// put caches a private copy of rec. A record that cannot be copied is not
// cached, so the next read decodes the file again.
func (c *recordCache) put(path string, id statIdentity, rec *Record) {
	cp, ok := deepCopy(rec)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !ok {
		delete(c.m, path)
		return
	}
	if c.m == nil {
		c.m = map[string]cachedRecord{}
	}
	c.m[path] = cachedRecord{id: id, rec: cp}
}

func (c *recordCache) drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, path)
}

// deepCopy copies a record built from JSON-decoded values: exported struct
// fields, pointers, slices, maps, and scalars. It reports false for any other
// shape (an unexported field, a channel, a func), and the caller then does
// not cache.
func deepCopy(rec *Record) (*Record, bool) {
	out := reflect.New(reflect.TypeOf(*rec))
	if !copyValue(out.Elem(), reflect.ValueOf(rec).Elem()) {
		return nil, false
	}
	return out.Interface().(*Record), true
}

func copyValue(dst, src reflect.Value) bool {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return true
		}
		dst.Set(reflect.New(src.Type().Elem()))
		return copyValue(dst.Elem(), src.Elem())
	case reflect.Slice:
		if src.IsNil() {
			return true
		}
		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Len()))
		for i := range src.Len() {
			if !copyValue(dst.Index(i), src.Index(i)) {
				return false
			}
		}
	case reflect.Map:
		if src.IsNil() {
			return true
		}
		dst.Set(reflect.MakeMapWithSize(src.Type(), src.Len()))
		for it := src.MapRange(); it.Next(); {
			v := reflect.New(src.Type().Elem()).Elem()
			if !copyValue(v, it.Value()) {
				return false
			}
			dst.SetMapIndex(it.Key(), v)
		}
	case reflect.Struct:
		for i := range src.NumField() {
			if !src.Type().Field(i).IsExported() || !copyValue(dst.Field(i), src.Field(i)) {
				return false
			}
		}
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.UnsafePointer:
		return false
	default:
		dst.Set(src)
	}
	return true
}
