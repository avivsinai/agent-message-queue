// Package dialog carries harness permission dialogs to Buzz and answers back.
// A harness seam (a hook, an app-server client, a bridge) opens a request
// for a live binding; the Buzz agent posts it and writes the owner's answer;
// the seam applies the answer, unless the terminal answered first. The
// first create of resolved/<id>.json decides how a dialog ended.
//
//	<binding store dir>/dialogs/<binding name>/
//	  requests/<id>.json  the seam: one open dialog
//	  answers/<id>.json   the Buzz agent: the owner's answer
//	  resolved/<id>.json  whoever closed the dialog first
//	  events/<event>      the Buzz agent: the one dialog an owner message answers
//	  owner.json          the Buzz agent: whose messages command the session
//	  commands/<event>    the seat's wake: an owner command it took, once
//
// Directories are 0700 and files 0600. A request is written whole by
// rename; an answer, a resolution and an event binding are written whole and
// create-exclusive, so the first writer wins and a reader never sees a
// partial file. An event binding is never removed.
package dialog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// Option is one choice the dialog offers.
type Option struct {
	N     int    `json:"n"`              // 1-based, as shown in Buzz
	Label string `json:"label"`          // "Allow once", "Allow this session", "Deny", or the harness's own label
	Value string `json:"value"`          // harness-specific decision token the seam applies
	Deny  bool   `json:"deny,omitempty"` // the option ❌ selects
}

// Request is one open dialog.
type Request struct {
	ID       string    `json:"id"`                  // unique, filename-safe (NewID)
	Harness  string    `json:"harness"`             // claude | codex | pi | acp
	Session  string    `json:"session"`             // native session id when known
	Tool     string    `json:"tool"`                // e.g. Bash, Edit, mcp__srv__tool, exec, apply_patch
	Text     string    `json:"text"`                // the dialog body as Buzz shows it
	Options  []Option  `json:"options"`             // empty for a free-text dialog without choices
	FreeText bool      `json:"free_text,omitempty"` // any reply text is the answer
	OpenedAt time.Time `json:"opened_at"`
	PID      int       `json:"pid"` // the waiting seam process
}

// Answer is the owner's answer from Buzz.
type Answer struct {
	ID     string    `json:"id"`
	Option int       `json:"option,omitempty"` // Option.N chosen
	Text   string    `json:"text,omitempty"`   // free-text answer
	By     string    `json:"by"`               // "buzz"
	At     time.Time `json:"at"`
	// Event is the signed owner message that typed this answer; "" for a
	// reaction. One message answers at most one dialog (BindEvent).
	Event string `json:"event,omitempty"`
}

// Resolution outcomes and closers.
const (
	OutcomeAllowed  = "allowed"
	OutcomeDenied   = "denied"
	OutcomeAnswered = "answered"
	OutcomeTerminal = "terminal"
	OutcomeExpired  = "expired"

	ByBuzz     = "buzz"
	ByTerminal = "terminal"
	ByTimeout  = "timeout"
)

// Resolved records how a dialog ended.
type Resolved struct {
	ID      string    `json:"id"`
	Outcome string    `json:"outcome"`         // OutcomeAllowed, OutcomeDenied, OutcomeAnswered, OutcomeTerminal or OutcomeExpired
	Label   string    `json:"label,omitempty"` // chosen option label, for the Buzz edit
	By      string    `json:"by"`              // ByBuzz, ByTerminal or ByTimeout
	At      time.Time `json:"at"`
}

// maxFileBytes bounds every dialog file read.
const maxFileBytes = 256 << 10

var idRe = regexp.MustCompile(`^d-[0-9a-f]{32}$`)

// NewID returns a fresh dialog id: "d-" and 32 hex characters.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "d-" + hex.EncodeToString(b[:])
}

// Root is the directory that holds every binding's dialogs directory.
func Root() (string, error) {
	path, err := binding.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "dialogs"), nil
}

// DirFor returns the dialogs directory of a binding, creating it and its
// requests/, answers/, resolved/ and events/ subdirectories.
func DirFor(bindingName string) (string, error) {
	if bindingName != binding.LegacyName {
		if err := binding.ValidName(bindingName); err != nil {
			return "", err
		}
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, bindingName)
	for _, sub := range []string{"requests", "answers", "resolved", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Owner is whose Buzz messages act on a binding's session: the owner's key,
// the channel its commands come from, and the session identity the binding
// had when the Buzz agent wrote the record. The Buzz agent writes it when
// it takes an owner command; the seat that types the command verifies the
// signed message against it, as a seam trusts an answer this agent wrote.
type Owner struct {
	PubKey  string `json:"pubkey"`
	Channel string `json:"channel"`
	Target  string `json:"target"`
}

const ownerFile = "owner.json"

// WriteOwner replaces the owner record of the dialogs directory dir.
func WriteOwner(dir string, o Owner) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = fsq.WriteStateFile(filepath.Dir(dir), filepath.Join(filepath.Base(dir), ownerFile), raw, 0o600)
	return err
}

// ReadOwner reads the owner record of the binding bindingName. It creates
// nothing; a binding that never took a command has none (os.ErrNotExist).
func ReadOwner(bindingName string) (Owner, error) {
	if bindingName != binding.LegacyName {
		if err := binding.ValidName(bindingName); err != nil {
			return Owner{}, err
		}
	}
	root, err := Root()
	if err != nil {
		return Owner{}, err
	}
	var o Owner
	found, err := readJSON(filepath.Join(root, bindingName, ownerFile), &o)
	switch {
	case err != nil:
		return Owner{}, err
	case !found:
		return Owner{}, os.ErrNotExist
	}
	return o, nil
}

// TakeCommand records, once, that the seat of binding bindingName took the
// owner command of event, and reports whether this call did; false means it
// was taken before. Records older than keep are removed first: keep must
// outlast every event the seat still accepts.
func TakeCommand(bindingName, event string, now time.Time, keep time.Duration) (bool, error) {
	if !eventRe.MatchString(event) {
		return false, fmt.Errorf("invalid event id %q", event)
	}
	dir, err := DirFor(bindingName)
	if err != nil {
		return false, err
	}
	taken := filepath.Join(dir, "commands")
	if err := os.MkdirAll(taken, 0o700); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(taken)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > keep {
			_ = os.Remove(filepath.Join(taken, e.Name()))
		}
	}
	f, err := os.OpenFile(filepath.Join(taken, event), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := f.Close(); err != nil {
		return true, err
	}
	// The record is dated by the caller's clock, the clock that prunes it.
	return true, os.Chtimes(filepath.Join(taken, event), now, now)
}

// ForgetCommand removes the record that the seat of binding bindingName
// took event. Only a seat that wrote nothing of the command forgets it, so
// the command can be typed later.
func ForgetCommand(bindingName, event string) error {
	if !eventRe.MatchString(event) {
		return fmt.Errorf("invalid event id %q", event)
	}
	root, err := Root()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(root, bindingName, "commands", event))
}

// Open writes requests/<id>.json.
func Open(dir string, r Request) error {
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("dialog id %q is not one NewID mints", r.ID)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = fsq.WriteStateFile(filepath.Dir(dir), filepath.Join(filepath.Base(dir), "requests", r.ID+".json"), raw, 0o600)
	return err
}

// Requests returns the open requests, those with no resolved/<id>.json,
// oldest first. An unreadable request file is skipped.
func Requests(dir string) ([]Request, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "requests"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idRe.MatchString(id) {
			continue
		}
		if _, done, _ := ReadResolved(dir, id); done {
			continue
		}
		var r Request
		if found, err := readJSON(filepath.Join(dir, "requests", e.Name()), &r); err != nil || !found || r.ID != id {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out, nil
}

// WriteAnswer writes answers/<id>.json; false when an answer exists already.
func WriteAnswer(dir string, a Answer) (bool, error) {
	return createExclusive(filepath.Join(dir, "answers"), a.ID, a)
}

// eventRe matches a signed message id: 64 lowercase hex characters.
var eventRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BindEvent makes request id the one dialog that the owner message event
// answers, unless event is bound already, and returns the dialog it is bound
// to. The first binding wins and is never removed, so one message answers at
// most one dialog across workers and restarts.
func BindEvent(dir, event, id string) (string, error) {
	if !eventRe.MatchString(event) {
		return "", fmt.Errorf("event id %q is not 64 lowercase hex characters", event)
	}
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	sub := filepath.Join(dir, "events")
	won, err := linkExclusive(sub, event, []byte(id))
	if err != nil || won {
		return id, err
	}
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(sub, event), int64(len(id)))
	if err != nil {
		return "", err
	}
	if bound := string(raw); idRe.MatchString(bound) {
		return bound, nil
	}
	return "", fmt.Errorf("event %s binds no dialog id", event)
}

// RejectAnswer removes answers/<id>.json, so the dialog is open again and
// the owner's next answer can be written. A seam calls it for an answer that
// fits none of its options; an answer left in place would keep the dialog
// answered, and nothing would read the owner's next reply.
func RejectAnswer(dir, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	err := os.Remove(filepath.Join(dir, "answers", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Resolve writes resolved/<id>.json; false when the dialog was resolved first
// by someone else.
func Resolve(dir string, r Resolved) (bool, error) {
	return createExclusive(filepath.Join(dir, "resolved"), r.ID, r)
}

// Phase is where one dialog is in its lifecycle.
type Phase string

const (
	// PhaseOpen: the request waits for an answer.
	PhaseOpen Phase = "open"
	// PhaseAnswered: the owner's answer waits for the seam to apply it.
	PhaseAnswered Phase = "answered"
	// PhaseResolved: the dialog ended; how is in its Resolved.
	PhaseResolved Phase = "resolved"
)

// Lookup returns the phase of dialog id with the files that decide it:
// resolved when resolved/<id>.json exists, whatever else does; else answered
// when answers/<id>.json exists; else open when requests/<id>.json exists.
// The request is returned when its file still exists. An error wrapping
// os.ErrNotExist means none of the three files exists. A resolution is
// authoritative: a leftover answer or request that cannot be read never
// hides it, so those are only best-effort details of a resolved dialog.
func Lookup(dir, id string) (Phase, Request, *Answer, *Resolved, error) {
	var (
		req Request
		ans Answer
		res Resolved
	)
	resolved, err := readID(dir, "resolved", id, &res)
	if err != nil {
		return "", Request{}, nil, nil, err
	}
	if resolved {
		var a *Answer
		if ok, err := readID(dir, "answers", id, &ans); err == nil && ok {
			a = &ans
		}
		if ok, err := readID(dir, "requests", id, &req); err != nil || !ok {
			req = Request{}
		}
		return PhaseResolved, req, a, &res, nil
	}
	answered, err := readID(dir, "answers", id, &ans)
	if err != nil {
		return "", Request{}, nil, nil, err
	}
	requested, err := readID(dir, "requests", id, &req)
	if err != nil {
		return "", Request{}, nil, nil, err
	}
	var a *Answer
	if answered {
		a = &ans
	}
	switch {
	case answered:
		return PhaseAnswered, req, a, nil, nil
	case requested:
		return PhaseOpen, req, nil, nil, nil
	}
	return "", Request{}, nil, nil, fmt.Errorf("dialog %s: %w", id, os.ErrNotExist)
}

// ReadResolved reads resolved/<id>.json; ok=false when the dialog is open.
func ReadResolved(dir, id string) (Resolved, bool, error) {
	var r Resolved
	ok, err := readID(dir, "resolved", id, &r)
	return r, ok, err
}

// FindBinding returns the live binding name that serves this seam: a
// mailbox binding with Root==root and Handle==handle, or a binding whose
// NativeSession or Codex app-server thread is session. ("", nil) when none
// does. A binding proven ended serves nothing.
func FindBinding(root, handle, session string) (string, error) {
	list, err := binding.List()
	if err != nil {
		if errors.Is(err, binding.ErrNone) || errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	root = canonical(root)
	for _, b := range list {
		mailbox := b.Mailbox() && root != "" && handle != "" && b.Handle == handle && canonical(b.Root) == root
		native := session != "" && (b.NativeSession == session || b.AppServer.Thread == session)
		if !mailbox && !native {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Ended {
			continue
		}
		return b.Name, nil
	}
	return "", nil
}

// ParseAnswer maps a Buzz reply to an answer: "N" picks option N;
// "yes"/"y"/"approve"/"✅" picks option 1; "no"/"n"/"reject"/"❌" picks the
// Deny option; any other non-empty text is a free-text answer when
// r.FreeText. ok=false when the reply is not an answer.
func ParseAnswer(r Request, reply string) (Answer, bool) {
	text := strings.TrimSpace(reply)
	word := strings.ToLower(strings.TrimRight(strings.TrimSuffix(text, "️"), ".!"))
	pick := func(n int) (Answer, bool) {
		return Answer{ID: r.ID, Option: n, By: ByBuzz, At: time.Now().UTC()}, true
	}
	if n, err := strconv.Atoi(word); err == nil {
		for _, o := range r.Options {
			if o.N == n {
				return pick(n)
			}
		}
	}
	switch word {
	case "yes", "y", "approve", "✅":
		for _, o := range r.Options {
			if o.N == 1 {
				return pick(1)
			}
		}
	case "no", "n", "reject", "❌":
		for _, o := range r.Options {
			if o.Deny {
				return pick(o.N)
			}
		}
	}
	if r.FreeText && text != "" {
		return Answer{ID: r.ID, Text: text, By: ByBuzz, At: time.Now().UTC()}, true
	}
	return Answer{}, false
}

// canonical is root with symlinks resolved, so two spellings of one root
// match; the clean path when it does not resolve.
func canonical(root string) string {
	if root == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
}

// createExclusive writes v whole to <sub>/<id>.json unless that file
// exists: the content goes to a temp file first and is hard-linked into
// place, so the link either creates the complete file or fails.
func createExclusive(sub, id string, v any) (bool, error) {
	if !idRe.MatchString(id) {
		return false, fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	return linkExclusive(sub, id+".json", raw)
}

// linkExclusive writes raw to sub/name whole: false when sub/name exists.
func linkExclusive(sub, name string, raw []byte) (bool, error) {
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(sub, "."+name+".tmp-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmp.Name(), filepath.Join(sub, name)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	_ = fsq.SyncDir(sub)
	return true, nil
}

func readID(dir, sub, id string, v any) (bool, error) {
	if !idRe.MatchString(id) {
		return false, fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	return readJSON(filepath.Join(dir, sub, id+".json"), v)
}

func readJSON(path string, v any) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, err
	}
	return true, nil
}
