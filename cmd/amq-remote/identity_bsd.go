//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// lsofCLI runs lsof for one pid's txt records with a C locale. The field
// output (-FpfDin) carries pid, file descriptor, device, inode, and name.
var lsofCLI = func(pid int) ([]byte, error) {
	cmd := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-FpfDin")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// processIdentityOS reads a running process's executable identity on macOS
// (and the BSDs, which share lsof):
// the txt record of lsof, whose n field is the executable vnode path and
// whose i and D fields carry its inode and device. A record whose name
// basename is not amq-acp is ignored, so dyld and other text records never
// decide. No matching record means the identity is unknown, and doctor
// skips the process.
func processIdentityOS(pid int) (execIdentity, error) {
	raw, err := lsofCLI(pid)
	if err != nil || len(raw) == 0 {
		return execIdentity{}, errors.New("no lsof txt record")
	}
	var dev, ino uint64
	var haveDev, haveIno bool
	name := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			// New file record: a txt record is complete when its name,
			// device, and inode were all seen for this f-block.
			if haveDev && haveIno && strings.EqualFold(filepath.Base(name), "amq-acp") {
				return execIdentity{Dev: dev, Inode: ino}, nil
			}
			dev, ino = 0, 0
			haveDev, haveIno = false, false
			name = ""
		case 'D':
			if v, err := strconv.ParseUint(strings.TrimPrefix(line, "D"), 0, 64); err == nil {
				dev, haveDev = v, true
			}
		case 'i':
			if v, err := strconv.ParseUint(strings.TrimPrefix(line, "i"), 10, 64); err == nil {
				ino, haveIno = v, true
			}
		case 'n':
			name = strings.TrimPrefix(line, "n")
		}
	}
	// The last record may still be complete.
	if haveDev && haveIno && strings.EqualFold(filepath.Base(name), "amq-acp") {
		return execIdentity{Dev: dev, Inode: ino}, nil
	}
	return execIdentity{}, errors.New("no amq-acp txt record")
}
