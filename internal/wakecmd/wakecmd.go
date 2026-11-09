// Package wakecmd is the queue of owner slash commands that a live wake
// types into its seat's terminal. amq-acp enqueues a command only from the
// owner's verified Buzz message; the wake types it, as its own line followed
// by Enter, only while no harness dialog is open, oldest first.
//
// The queue is a directory under the agent directory. A request is
// <id>.req; the wake claims it by renaming it to <id>.typing, and every
// state the owner is told about is <id>.out. An id is a fixed-width
// enqueue time and the event key, so names sort in arrival order and one
// event enqueues at most once while its outcome is kept.
package wakecmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DirName is the queue directory inside the agent directory.
const DirName = ".wake.command"

// MaxBytes bounds one command line.
const MaxBytes = 512

// KeepOutcomes is how long an outcome stays after its request is gone. It
// outlives any event an owner gate still accepts, so a redelivered event
// finds its earlier request and is not typed twice.
const KeepOutcomes = time.Hour

// Outcome states written in <id>.out.
const (
	Typed   = "typed"
	Queued  = "queued"
	Refused = "refused"
)

const (
	reqSuffix    = ".req"
	typingSuffix = ".typing"
	outSuffix    = ".out"
)

// ErrDuplicate is an event that already has a request or an outcome.
var ErrDuplicate = errors.New("this message was already taken as a command")

var keyRe = regexp.MustCompile(`^[0-9a-z]{1,64}$`)

var idRe = regexp.MustCompile(`^[0-9]{20}-[0-9a-z]{1,64}$`)

// Check is the one rule for a command line: it starts with "/", it is one
// line of at most MaxBytes of valid UTF-8, and every character is printable
// (a space is the only white space allowed), so no control or escape byte
// reaches the terminal.
func Check(text string) error {
	switch {
	case !strings.HasPrefix(text, "/"):
		return errors.New("a command starts with /")
	case len(text) > MaxBytes:
		return fmt.Errorf("a command is at most %d bytes", MaxBytes)
	case !utf8.ValidString(text):
		return errors.New("a command must be valid UTF-8")
	}
	for _, r := range text {
		if r == '\n' || r == '\r' {
			return errors.New("a command is one line")
		}
		if !unicode.IsPrint(r) {
			return errors.New("a command may hold printable characters only")
		}
	}
	return nil
}

// Request is one queued command.
type Request struct {
	ID   string
	Text string
}

// Dir is the queue directory of the agent directory agentDir.
func Dir(agentDir string) string { return filepath.Join(agentDir, DirName) }

// Enqueue adds text as the newest request of agentDir for the event key and
// returns its id. The agent directory must exist; the queue directory is
// created mode 0700.
func Enqueue(agentDir, key, text string, now time.Time) (string, error) {
	if err := Check(text); err != nil {
		return "", err
	}
	if !keyRe.MatchString(key) {
		return "", fmt.Errorf("invalid command key %q", key)
	}
	dir, err := openDir(agentDir, true)
	if err != nil {
		return "", err
	}
	names, err := listNames(dir)
	if err != nil {
		return "", err
	}
	prune(dir, names, now)
	for _, name := range names {
		if _, k, ok := splitName(name); ok && k == key {
			return "", ErrDuplicate
		}
	}
	id := fmt.Sprintf("%020d-%s", now.UnixNano(), key)
	if err := writeAtomic(dir, id+reqSuffix, []byte(text)); err != nil {
		return "", err
	}
	return id, nil
}

// Outcome reads the state of request id: Typed, Queued or Refused and its
// reason, or ok false when the wake has said nothing yet.
func Outcome(agentDir, id string) (state, reason string, ok bool) {
	if !idRe.MatchString(id) {
		return "", "", false
	}
	raw, err := readSmall(filepath.Join(Dir(agentDir), id+outSuffix))
	if err != nil {
		return "", "", false
	}
	state, reason, _ = strings.Cut(strings.TrimSpace(string(raw)), ": ")
	return state, reason, true
}

// Withdraw removes request id while no wake has claimed it, and reports
// whether it did. A request the wake claimed is typed or answered.
func Withdraw(agentDir, id string) bool {
	if !idRe.MatchString(id) {
		return false
	}
	return os.Remove(filepath.Join(Dir(agentDir), id+reqSuffix)) == nil
}

// Pending returns the unclaimed requests of agentDir, oldest first. A
// request whose text fails Check is answered Refused and dropped.
func Pending(agentDir string) ([]Request, error) {
	dir, err := openDir(agentDir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names, err := listNames(dir)
	if err != nil {
		return nil, err
	}
	var reqs []Request
	for _, name := range names {
		id, ok := strings.CutSuffix(name, reqSuffix)
		if !ok || !idRe.MatchString(id) {
			continue
		}
		raw, err := readSmall(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		text := string(raw)
		if err := Check(text); err != nil {
			if Claim(agentDir, id) {
				Finish(agentDir, id, Refused, err.Error())
			}
			continue
		}
		reqs = append(reqs, Request{ID: id, Text: text})
	}
	return reqs, nil
}

// Claim takes request id for typing; false means it was withdrawn.
func Claim(agentDir, id string) bool {
	dir := Dir(agentDir)
	return os.Rename(filepath.Join(dir, id+reqSuffix), filepath.Join(dir, id+typingSuffix)) == nil
}

// Release returns a claimed request to the queue in its place.
func Release(agentDir, id string) {
	dir := Dir(agentDir)
	_ = os.Rename(filepath.Join(dir, id+typingSuffix), filepath.Join(dir, id+reqSuffix))
}

// Mark records state for request id, unless that is already its outcome.
func Mark(agentDir, id, state, reason string) {
	line := state
	if reason != "" {
		line += ": " + reason
	}
	if s, r, ok := Outcome(agentDir, id); ok && s == state && r == reason {
		return
	}
	_ = writeAtomic(Dir(agentDir), id+outSuffix, []byte(line+"\n"))
}

// Finish records the final state of a claimed request and removes it.
func Finish(agentDir, id, state, reason string) {
	Mark(agentDir, id, state, reason)
	_ = os.Remove(filepath.Join(Dir(agentDir), id+typingSuffix))
}

// splitName parses <id>.<suffix> into its id and event key.
func splitName(name string) (id, key string, ok bool) {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 || !idRe.MatchString(name[:dot]) {
		return "", "", false
	}
	id = name[:dot]
	return id, id[21:], true
}

// prune removes outcomes older than KeepOutcomes whose request is gone.
func prune(dir string, names []string, now time.Time) {
	for _, name := range names {
		id, ok := strings.CutSuffix(name, outSuffix)
		if !ok || slices.Contains(names, id+reqSuffix) || slices.Contains(names, id+typingSuffix) {
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil && now.Sub(info.ModTime()) > KeepOutcomes {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// openDir returns the queue directory of agentDir, a real directory and
// never a symlink, creating it when create is set.
func openDir(agentDir string, create bool) (string, error) {
	dir := Dir(agentDir)
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create command queue: %w", err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("command queue %s is not a directory", dir)
	}
	return dir, nil
}

func listNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// readSmall reads a regular file of at most MaxBytes+1 bytes, never through
// a symlink.
func readSmall(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBytes+1 {
		return nil, fmt.Errorf("%s is not a command file", path)
	}
	return os.ReadFile(path)
}

// writeAtomic installs data as dir/name, mode 0600, by an exclusive temp
// file and a rename.
func writeAtomic(dir, name string, data []byte) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(nonce[:]))
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
