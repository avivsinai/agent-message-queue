package requests

import (
	"fmt"
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
		return deepCopy(e.rec)
	}
	return nil
}

func (c *recordCache) put(path string, id statIdentity, rec *Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cachedRecord{}
	}
	c.m[path] = cachedRecord{id: id, rec: deepCopy(rec)}
}

func (c *recordCache) drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, path)
}

// deepCopy copies a record built from JSON-decoded values: exported struct
// fields, pointers, slices, maps, and scalars.
func deepCopy(rec *Record) *Record {
	out := reflect.New(reflect.TypeOf(*rec))
	copyValue(out.Elem(), reflect.ValueOf(rec).Elem())
	return out.Interface().(*Record)
}

func copyValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.New(src.Type().Elem()))
		copyValue(dst.Elem(), src.Elem())
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Len()))
		for i := range src.Len() {
			copyValue(dst.Index(i), src.Index(i))
		}
	case reflect.Map:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.MakeMapWithSize(src.Type(), src.Len()))
		for it := src.MapRange(); it.Next(); {
			v := reflect.New(src.Type().Elem()).Elem()
			copyValue(v, it.Value())
			dst.SetMapIndex(it.Key(), v)
		}
	case reflect.Struct:
		for i := range src.NumField() {
			if !src.Type().Field(i).IsExported() {
				panic(fmt.Sprintf("requests: cannot copy unexported field %s.%s", src.Type(), src.Type().Field(i).Name))
			}
			copyValue(dst.Field(i), src.Field(i))
		}
	default:
		dst.Set(src)
	}
}
