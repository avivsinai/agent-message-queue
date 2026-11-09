// Package wakecmd is the queue of owner slash commands that a live wake
// types into its seat's terminal. amq-acp enqueues a command only from the
// owner's verified Buzz message; the wake types it, as its own line followed
// by Enter, only while no harness dialog is open, oldest first.
//
// The queue is a directory under the agent directory. A request is
// <id>.req, which carries the owner's signed message itself: a file in the
// queue is never proof on its own, and the wake verifies the message again
// (Verify) before it types. The wake claims a request by renaming it to
// <id>.typing, and every state the owner is told about is <id>.out. An id
// is a fixed-width enqueue time and the event key, so names sort in arrival
// order. <key>.key, created exclusively before the request, takes each
// event at most once.
package wakecmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzevent"
)

// DirName is the queue directory inside the agent directory.
const DirName = ".wake.command"

// MaxBytes bounds one command line.
const MaxBytes = 512

// MaxRequestBytes bounds one request file: the command and its signed
// message.
const MaxRequestBytes = 4 << 10

// The queue holds at most MaxPending requests and MaxPendingBytes in all.
const (
	MaxPending      = 16
	MaxPendingBytes = 16 << 10
)

// Expiry is how long after the owner signed a command it may still be
// typed. A command held behind a dialog for longer is refused: the
// terminal is no longer the context the owner typed it for.
const Expiry = 10 * time.Minute

// KeepOutcomes is how long an outcome and an event key stay after their
// request is gone. It outlives any event an owner gate still accepts, so a
// redelivered event finds its key and is not typed twice.
const KeepOutcomes = time.Hour

// Outcome states written in <id>.out.
const (
	Typed   = "typed"
	Queued  = "queued"
	Refused = "refused"
)

// File suffixes of the queue.
const (
	ReqSuffix    = ".req"
	TypingSuffix = ".typing"
	OutSuffix    = ".out"
	keySuffix    = ".key"
)

var (
	// ErrDuplicate is an event that was already taken as a command.
	ErrDuplicate = errors.New("this message was already taken as a command")
	// ErrFull is a queue that holds MaxPending requests or MaxPendingBytes.
	ErrFull = fmt.Errorf("%d commands already wait for the terminal", MaxPending)
	// ErrExpired is a command signed more than Expiry ago.
	ErrExpired = errors.New("expired while a dialog was open")
)

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
	// ID is the request's file name without its suffix.
	ID string `json:"-"`
	// Target is the session identity of the binding the command was taken
	// for; only the seat of that session types it.
	Target string `json:"target"`
	// Text is the command line: the signed message's content without its
	// leading mentions.
	Text string `json:"text"`
	// Event is the owner's signed message as the Buzz agent verified it.
	Event json.RawMessage `json:"event"`
}

// Parse reads the request that file id holds.
func Parse(id string, raw []byte) (Request, error) {
	req := Request{ID: id}
	if len(raw) > MaxRequestBytes {
		return req, errors.New("the request is too large")
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, errors.New("the request is not a command request")
	}
	return req, nil
}

// Verify is the rule a wake applies before it types req in the seat of
// session target: req was taken for target, and its event is a plaintext
// message in channel that owner signed, whose command line is req.Text,
// signed no more than Expiry before now. Authenticity is checked first, so
// ErrExpired is only ever said of the owner's own command.
func Verify(req Request, target, owner, channel string, now time.Time) error {
	if target == "" || req.Target != target {
		return errors.New("the command was taken for another session")
	}
	var evt nostr.Event
	if err := json.Unmarshal(req.Event, &evt); err != nil {
		return errors.New("the command carries no signed message")
	}
	if err := buzzevent.Verify(evt, channel); err != nil {
		return fmt.Errorf("the command's message does not verify: %w", err)
	}
	if owner == "" || evt.PubKey.Hex() != owner {
		return errors.New("the command's message is not the owner's")
	}
	if buzzevent.DropMentions(evt.Content) != req.Text {
		return errors.New("the command differs from the owner's message")
	}
	if err := Check(req.Text); err != nil {
		return err
	}
	if now.Sub(evt.CreatedAt.Time()) > Expiry {
		return ErrExpired
	}
	return nil
}

// RequestID is the id of a request file name; ok is false for any other
// name.
func RequestID(name string) (id string, ok bool) {
	id, ok = strings.CutSuffix(name, ReqSuffix)
	return id, ok && idRe.MatchString(id)
}

// EnqueuedAt is when request id was enqueued.
func EnqueuedAt(id string) time.Time {
	if !idRe.MatchString(id) {
		return time.Time{}
	}
	nanos, err := strconv.ParseInt(id[:20], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// OutcomeLine is the content of an <id>.out file.
func OutcomeLine(state, reason string) []byte {
	if reason != "" {
		state += ": " + reason
	}
	return []byte(state + "\n")
}

// Dir is the queue directory of the agent directory agentDir.
func Dir(agentDir string) string { return filepath.Join(agentDir, DirName) }

// Enqueue adds req as the newest request of agentDir for the event key and
// returns its id. The agent directory must exist; the queue directory is
// created mode 0700. The key is reserved before the request is written, so
// two processes never enqueue one event twice.
func Enqueue(agentDir, key string, req Request, now time.Time) (string, error) {
	if err := Check(req.Text); err != nil {
		return "", err
	}
	if !keyRe.MatchString(key) {
		return "", fmt.Errorf("invalid command key %q", key)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if len(raw) > MaxRequestBytes {
		return "", errors.New("the command's message is too large")
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
	count, total := 0, len(raw)
	for _, name := range names {
		if _, ok := RequestID(name); !ok {
			continue
		}
		count++
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			total += int(info.Size())
		}
	}
	if count >= MaxPending || total > MaxPendingBytes {
		return "", ErrFull
	}
	f, err := os.OpenFile(filepath.Join(dir, key+keySuffix), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return "", ErrDuplicate
	}
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	id := fmt.Sprintf("%020d-%s", now.UnixNano(), key)
	if err := writeAtomic(dir, id+ReqSuffix, raw); err != nil {
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
	raw, err := readSmall(filepath.Join(Dir(agentDir), id+OutSuffix))
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
	return os.Remove(filepath.Join(Dir(agentDir), id+ReqSuffix)) == nil
}

// prune removes outcomes and event keys older than KeepOutcomes whose
// request, claimed or not, is gone.
func prune(dir string, names []string, now time.Time) {
	held := make(map[string]bool)
	for _, name := range names {
		for _, suffix := range []string{ReqSuffix, TypingSuffix} {
			if id, ok := strings.CutSuffix(name, suffix); ok && idRe.MatchString(id) {
				held[id] = true
				held[id[21:]+keySuffix] = true
			}
		}
	}
	for _, name := range names {
		id, isOut := strings.CutSuffix(name, OutSuffix)
		switch {
		case isOut && held[id], strings.HasSuffix(name, keySuffix) && held[name]:
			continue
		case !isOut && !strings.HasSuffix(name, keySuffix):
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
