package mailprompt

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Hit is one inbox file whose header answers a prompt.
type Hit struct {
	Filename string
	Header   format.Header
	Created  time.Time
}

// Scan lists one inbox box and keeps each file's header by name. A
// delivered maildir file never changes, so each name's header is read once:
// a poll costs one directory listing plus the headers of names it has not
// seen, not a read of every file. A header that cannot be parsed
// is remembered as nil and the file is left for drain and the DLQ.
//
// With stat set, a poll of a box that has not changed since the last full
// listing costs one stat: the listing is skipped while the directory is the
// same one with the same mtime, that mtime is at least inboxMtimeSlack older
// than the listing, and no header read was left for a retry. Every file
// added or removed by name changes the directory's mtime.
type Scan struct {
	stat    func() (os.FileInfo, error)
	list    func() ([]os.DirEntry, error)
	header  func(name string) (format.Header, error)
	read    func(name string) (format.Message, error)
	headers map[string]*format.Header
	// listed is the directory as stat saw it just before the last full
	// listing, and listedAt when that listing began; nil forces a listing.
	listed   os.FileInfo
	listedAt time.Time
}

// inboxMtimeSlack covers the coarsest directory mtime a supported file
// system keeps: a change within the same mtime tick as the last listing may
// postdate it, so such a listing is never trusted.
const inboxMtimeSlack = 2 * time.Second

// NewScan is a Scan of the directory dir.
func NewScan(dir string) *Scan {
	return &Scan{
		stat:    func() (os.FileInfo, error) { return os.Stat(dir) },
		list:    func() ([]os.DirEntry, error) { return os.ReadDir(dir) },
		header:  func(name string) (format.Header, error) { return format.ReadHeaderFile(filepath.Join(dir, name)) },
		read:    func(name string) (format.Message, error) { return format.ReadMessageFile(filepath.Join(dir, name)) },
		headers: map[string]*format.Header{},
	}
}

// newRootScan is a Scan of the root-relative dir whose every
// read goes through root.
func newRootScan(root *fsq.DeliveryRoot, dir string) *Scan {
	return &Scan{
		list: func() ([]os.DirEntry, error) { return root.ReadDir(dir) },
		header: func(name string) (format.Header, error) {
			return format.ReadHeaderFileRoot(root, filepath.Join(dir, name))
		},
		read: func(name string) (format.Message, error) {
			return format.ReadMessageFileRoot(root, filepath.Join(dir, name))
		},
		headers: map[string]*format.Header{},
	}
}

// Replies refreshes the scan and returns its matches.
func (sc *Scan) Replies(handle, threadID, promptID string, since time.Time) ([]Hit, error) {
	if err := sc.Refresh(); err != nil {
		return nil, err
	}
	return sc.match(handle, threadID, promptID, since), nil
}

// Refresh lists the box once, reads the headers of names not seen yet, and
// forgets names that are gone. A missing dir is empty.
func (sc *Scan) Refresh() error {
	var dir os.FileInfo
	if sc.stat != nil {
		dir, _ = sc.stat()
		if dir != nil && sc.listed != nil && os.SameFile(dir, sc.listed) && dir.ModTime().Equal(sc.listed.ModTime()) &&
			sc.listedAt.Sub(dir.ModTime()) >= inboxMtimeSlack {
			return nil
		}
	}
	sc.listed, sc.listedAt = nil, time.Now()
	entries, err := sc.list()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	retry := false
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		listed[name] = true
		if _, known := sc.headers[name]; known {
			continue
		}
		h, err := sc.header(name)
		var pathErr *fs.PathError
		switch {
		case err == nil:
			sc.headers[name] = &h
		case errors.Is(err, fsq.ErrDeliveryRootChanged):
			return err
		case errors.As(err, &pathErr):
			// gone or unreadable now; try again on the next refresh
			retry = true
		default:
			sc.headers[name] = nil
		}
	}
	for name := range sc.headers {
		if !listed[name] {
			delete(sc.headers, name)
		}
	}
	if !retry {
		sc.listed = dir
	}
	return nil
}

// match returns, oldest first, the files from the last refresh that come
// from handle on threadID, ref promptID, and were created no earlier than
// since.
func (sc *Scan) match(handle, threadID, promptID string, since time.Time) []Hit {
	var hits []Hit
	for name, header := range sc.headers {
		if header == nil || !slices.Contains(header.Refs, promptID) {
			continue
		}
		if hit, ok := ReplyTo(name, header, handle, threadID, since); ok {
			hits = append(hits, hit)
		}
	}
	SortHits(hits)
	return hits
}

// ReplyTo reports whether header is a reply from handle on threadID created
// no earlier than since.
func ReplyTo(name string, header *format.Header, handle, threadID string, since time.Time) (Hit, bool) {
	if header.From != handle || header.Thread != threadID {
		return Hit{}, false
	}
	created, err := time.Parse(time.RFC3339Nano, header.Created)
	if err != nil || created.Before(since) {
		return Hit{}, false
	}
	return Hit{Filename: name, Header: *header, Created: created}, true
}

// SortHits orders hits oldest first.
func SortHits(hits []Hit) {
	slices.SortFunc(hits, func(a, b Hit) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Filename, b.Filename)
	})
}

// Read reads the message of file name from the scanned directory.
func (sc *Scan) Read(name string) (format.Message, error) {
	return sc.read(name)
}

// Headers returns the headers of the last Refresh by file name; a nil header
// is a file whose header cannot be parsed. The map belongs to the Scan and
// is valid until the next Refresh.
func (sc *Scan) Headers() map[string]*format.Header {
	return sc.headers
}
