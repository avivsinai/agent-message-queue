//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// psListCLI runs ps with a C locale so comm and its spacing are stable.
var psListCLI = func() ([]byte, error) {
	cmd := exec.Command("ps", "-axo", "pid=,comm=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// listACPProcessesOS lists running amq-acp candidates through ps. comm is
// argv[0], not executable identity, so it only decides candidacy; the
// identity check decides. comm is everything after the pid, so a launch
// path with spaces stays whole. A ps failure is not a doctor failure:
// doctor reports only what it can see.
func listACPProcessesOS() ([]acpProcess, error) {
	raw, err := psListCLI()
	if err != nil {
		return nil, nil
	}
	var out []acpProcess
	for _, line := range strings.Split(string(raw), "\n") {
		pidStr, comm, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		comm = strings.TrimPrefix(comm, " ")
		if !strings.EqualFold(filepath.Base(comm), "amq-acp") {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		out = append(out, acpProcess{PID: pid, Name: comm})
	}
	return out, nil
}

// statIdentity maps a syscall stat record to the executable identity.
func statIdentity(info os.FileInfo) (execIdentity, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return execIdentity{}, false
	}
	return execIdentity{Dev: uint64(st.Dev), Inode: st.Ino}, true
}

// installedIdentityOS is the (dev, inode) of the resolved installed
// amq-acp. A file that cannot be stat'ed is no evidence.
func installedIdentityOS() (execIdentity, error) {
	path, err := installedACPPath()
	if err != nil {
		return execIdentity{}, err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return execIdentity{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return execIdentity{}, err
	}
	id, ok := statIdentity(info)
	if !ok {
		return execIdentity{}, errors.New("no stat identity for " + real)
	}
	return id, nil
}
