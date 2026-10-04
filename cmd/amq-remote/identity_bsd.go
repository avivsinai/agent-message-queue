//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// lsofCLI runs lsof for one pid's txt records with a C locale, bounded the
// same way as the other doctor probes. The field output (-Fpfin) carries
// pid, file descriptor, inode, and name.
var lsofCLI = func(pid int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fpfin")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// lsofTimeout bounds each lsof probe; a probe that outlives it yields no
// identity and the process is skipped, never guessed.
const lsofTimeout = 2 * time.Second

// processIdentityOS reads a running process's executable identity on macOS
// (and the BSDs, which share lsof): the FIRST txt record of the process is
// the program text, whose n field names the running executable and whose i
// field carries its inode. Matching is by full path, never by basename
// across records: if the first txt record's basename is not amq-acp, the
// process is not an amq-acp candidate and is skipped, no matter what other
// records name. No readable first record means the identity is unknown, and
// doctor skips the process.
func processIdentityOS(pid int) (execIdentity, error) {
	raw, err := lsofCLI(pid)
	if err != nil || len(raw) == 0 {
		return execIdentity{}, errors.New("no lsof txt record")
	}
	name := ""
	var ino uint64
	haveIno := false
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			if name != "" || haveIno {
				// The first txt record is the program text; later records
				// (dyld and other text) are never searched.
				if strings.EqualFold(filepath.Base(name), "amq-acp") {
					return execIdentity{Dev: anyDevice, Inode: ino}, nil
				}
				return execIdentity{}, errors.New("first txt record is not amq-acp")
			}
			name, ino, haveIno = "", 0, false
		case 'i':
			if v, perr := strconv.ParseUint(strings.TrimPrefix(line, "i"), 10, 64); perr == nil {
				ino, haveIno = v, true
			}
		case 'n':
			name = strings.TrimPrefix(line, "n")
		}
	}
	// The first record may end without a following f line.
	if strings.EqualFold(filepath.Base(name), "amq-acp") && haveIno {
		return execIdentity{Dev: anyDevice, Inode: ino}, nil
	}
	return execIdentity{}, errors.New("first txt record is not amq-acp")
}
