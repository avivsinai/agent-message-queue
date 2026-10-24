package usage

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"time"
)

var tsKey = []byte(`"timestamp":"`)

// lineTime dates a transcript line by its first "timestamp" string field.
func lineTime(line []byte) (time.Time, bool) {
	i := bytes.Index(line, tsKey)
	if i < 0 {
		return time.Time{}, false
	}
	rest := line[i+len(tsKey):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(rest[:j]))
	return t, err == nil
}

// seekWindow returns an offset at or before the first line dated at or after
// since. Transcripts are append-ordered, so a binary search over byte
// offsets finds it; each probe reads to the next newline and dates the first
// dated line after it. Below 1 MiB the search stops and reads from there.
func seekWindow(f *os.File, size int64, since time.Time, b *budget) int64 {
	lo, hi := int64(0), size
	for hi-lo > 1<<20 {
		mid := lo + (hi-lo)/2
		t, ok := firstTimeAfter(f, mid, b)
		if !ok || !t.Before(since) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return lo
}

// firstTimeAfter dates the first dated line that starts after off.
func firstTimeAfter(f *os.File, off int64, b *budget) (time.Time, bool) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return time.Time{}, false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	first := off > 0 // the line under off is partial
	for k := 0; k < 64; k++ {
		line, n, err := readLine(r)
		b.bytes -= n
		if !first && line != nil {
			if t, ok := lineTime(line); ok {
				return t, true
			}
		}
		first = false
		if err != nil {
			return time.Time{}, false
		}
	}
	return time.Time{}, false
}

// errLineTooLong marks a line over maxLine; it was consumed, not returned.
var errLineTooLong = errors.New("line over the length limit")

// readLine returns the next line without its newline, the bytes consumed,
// and io.EOF after the last line. A line over maxLine is consumed and
// returned as nil with errLineTooLong.
func readLine(r *bufio.Reader) ([]byte, int64, error) {
	var buf []byte
	var n int64
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			if len(buf)+len(chunk) > maxLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && len(chunk) == 0 && n == 0:
			return nil, 0, err
		case tooLong:
			if err == nil {
				err = errLineTooLong
			} else {
				err = errors.Join(errLineTooLong, err)
			}
			return nil, n, err
		}
		return bytes.TrimRight(buf, "\r\n"), n, err
	}
}

// scan reads path from off, calling fn for each line that holds one of
// keys, until EOF, the budget, or fn returning false. It reports whether it
// read to the end, and counts skipped long lines into skipped.
func scan(f *os.File, off int64, keys [][]byte, b *budget, skipped *int, fn func(line []byte) bool) bool {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return false
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		if b.spent() {
			return false
		}
		line, n, err := readLine(r)
		b.bytes -= n
		if errors.Is(err, errLineTooLong) {
			*skipped++
		}
		if line != nil && hasAny(line, keys) && !fn(line) {
			return true
		}
		switch {
		case errors.Is(err, io.EOF):
			return true
		case err != nil && !errors.Is(err, errLineTooLong):
			return false
		}
	}
}

func hasAny(line []byte, keys [][]byte) bool {
	for _, k := range keys {
		if bytes.Contains(line, k) {
			return true
		}
	}
	return false
}

// headLines bounds how far into a session file the reader looks for its
// identity fields.
const headLines = 32

// headField returns the first value field finds among the file's first
// lines.
func headField(f *os.File, b *budget, field func([]byte) (string, bool)) (string, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for k := 0; k < headLines; k++ {
		line, n, err := readLine(r)
		b.bytes -= n
		if line != nil {
			if v, ok := field(line); ok {
				return v, true
			}
		}
		if err != nil && !errors.Is(err, errLineTooLong) {
			return "", false
		}
	}
	return "", false
}
