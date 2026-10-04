//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// lsofCLI reads one pid's txt (executable) records with a C locale under
// the probe's total context. -FpfDin emits per-field records: pid, file
// descriptor (one 'f' line per file record), device (hex), inode, name.
var lsofCLI = func(ctx context.Context, pid int) ([]byte, error) {
	return runBounded(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-FpfDin")
}

// txtRecord is the fields collected for one lsof file record.
type txtRecord struct {
	name    string
	dev     uint64
	ino     uint64
	haveDev bool
	seen    bool // an f-line was seen for this record
}

// strict checks the review's bar: non-empty, absolute name whose basename
// is amq-acp, a non-zero inode, and a parseable device.
func (r txtRecord) strict() (execIdentity, error) {
	if r.name == "" || !filepath.IsAbs(r.name) {
		return execIdentity{}, errors.New("first txt record has no absolute name")
	}
	if !strings.EqualFold(filepath.Base(r.name), "amq-acp") {
		return execIdentity{}, errors.New("first txt record is not amq-acp")
	}
	if r.ino == 0 {
		return execIdentity{}, errors.New("first txt record has no inode")
	}
	if !r.haveDev {
		return execIdentity{}, errors.New("first txt record has no parseable device")
	}
	return execIdentity{Dev: r.dev, Inode: r.ino}, nil
}

// processIdentityOS reads a running process's executable identity on macOS
// (and the BSDs, which share lsof). The FIRST txt record is the program
// text and decides alone: it must be strict, otherwise the process is
// unknown. Never fall through to the second record, never guess.
func processIdentityOS(ctx context.Context, pid int) (execIdentity, error) {
	raw, err := lsofCLI(ctx, pid)
	if err != nil || len(raw) == 0 {
		return execIdentity{}, fmt.Errorf("no lsof txt record (lsof unavailable or timed out)")
	}
	var first, cur txtRecord
	inRecord := false // a txt record is open
	decided := false
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			if decided {
				continue
			}
			if inRecord {
				// The second f closes the FIRST record; it decides alone.
				decided = true
				break
			}
			cur = txtRecord{seen: true}
			inRecord = true
		case 'D':
			if inRecord && !decided {
				// lsof emits the device in hex with a 0x prefix; Go's
				// ParseUint does not accept the prefix, so strip it.
				d := strings.TrimPrefix(strings.TrimPrefix(line, "D"), "0x")
				if v, perr := strconv.ParseUint(d, 16, 64); perr == nil {
					cur.dev, cur.haveDev = v, true
				}
			}
		case 'i':
			if inRecord && !decided {
				if v, perr := strconv.ParseUint(strings.TrimPrefix(line, "i"), 10, 64); perr == nil {
					cur.ino = v
				}
			}
		case 'n':
			if inRecord && !decided {
				cur.name = strings.TrimPrefix(line, "n")
			}
		}
	}
	if inRecord {
		first = cur
	}
	if !first.seen {
		return execIdentity{}, errors.New("no lsof txt record (lsof unavailable or timed out)")
	}
	return first.strict()
}
