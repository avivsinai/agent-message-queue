//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const (
	coopNamedStoreClockSlack     = 2 * time.Second
	coopNamedTUIReadbackInterval = 500 * time.Millisecond
)

var (
	coopNamedTUIReadbackTimeout = 5 * time.Second
	coopNamedReadbackSleep      = time.Sleep

	// errCoopNamedStoreNoCandidate marks a locate scan that found no
	// candidate yet; the namer polls it instead of giving up at once. Errors
	// that carry it keep today's warning text and are recognized by Is.
	errCoopNamedStoreNoCandidate = errors.New("no candidate yet")
)

// coopNamedStoreNoCandidateError carries today's warning text and is
// recognized as the no-candidate case, so a poll treats it as "not yet"
// while the printed reminder stays exactly as it was.
type coopNamedStoreNoCandidateError struct{ msg string }

func (e coopNamedStoreNoCandidateError) Error() string { return e.msg }

func (e coopNamedStoreNoCandidateError) Is(target error) bool {
	return target == errCoopNamedStoreNoCandidate
}

func isCoopNamedNoCandidate(err error) bool {
	return errors.Is(err, errCoopNamedStoreNoCandidate)
}

type coopNamedStoreCandidate struct {
	storePath string
	name      string
	modTime   time.Time
}

type coopNamedStoreReader interface {
	locate(cwd string, execStart time.Time) (coopNamedStoreCandidate, error)
	readName(candidate coopNamedStoreCandidate) (string, error)
}

func coopNamedStoreReaderFor(binary string) (coopNamedStoreReader, bool) {
	switch launch.ProviderForExecutable(binary) {
	case launch.CursorProvider:
		return cursorNamedStoreReader{}, true
	default:
		return nil, false
	}
}

func selectCoopNamedStoreCandidate(candidates []coopNamedStoreCandidate, description string) (coopNamedStoreCandidate, error) {
	switch len(candidates) {
	case 0:
		return coopNamedStoreCandidate{}, coopNamedStoreNoCandidateError{msg: fmt.Sprintf("no new %s store candidate", description)}
	case 1:
		return candidates[0], nil
	default:
		return coopNamedStoreCandidate{}, fmt.Errorf("%d new %s store candidates are ambiguous", len(candidates), description)
	}
}

type cursorChatMeta struct {
	Title       string          `json:"title"`
	CWD         string          `json:"cwd"`
	CreatedAt   json.RawMessage `json:"createdAt"`
	CreatedAtMs json.RawMessage `json:"createdAtMs"`
}

type cursorNamedStoreReader struct{}

// cursorNamedStoreRoot looks in the Cursor CLI's configured directory,
// rather than falling back to a second store that may hold another chat.
func cursorNamedStoreRoot() (string, error) {
	if dir := os.Getenv("CURSOR_CONFIG_DIR"); strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, "chats"), nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); runtime.GOOS == "linux" && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, "cursor", "chats"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Cursor home: %w", err)
	}
	return filepath.Join(home, ".cursor", "chats"), nil
}

func (cursorNamedStoreReader) locate(cwd string, execStart time.Time) (coopNamedStoreCandidate, error) {
	root, err := cursorNamedStoreRoot()
	if err != nil {
		return coopNamedStoreCandidate{}, err
	}
	windowStart := execStart.Add(-coopNamedStoreClockSlack)
	var candidates []coopNamedStoreCandidate
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "meta.json" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		// A chat whose metadata cannot be read or parsed cannot be matched to
		// this cwd; skip it. The next poll sees it again once it is whole.
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		var meta cursorChatMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}
		if meta.CWD != cwd {
			return nil
		}
		createdAt, hasCreationTime, creationErr := cursorChatCreationTime(path, meta)
		if creationErr != nil {
			return fmt.Errorf("read Cursor chat creation time %q: %w", path, creationErr)
		}
		if !hasCreationTime || createdAt.Before(windowStart) {
			return nil
		}
		candidates = append(candidates, coopNamedStoreCandidate{
			storePath: path,
			name:      meta.Title,
			modTime:   info.ModTime(),
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The store root is created with the first chat, so an absent
			// store is the record-not-written-yet case, not a broken one.
			return coopNamedStoreCandidate{}, coopNamedStoreNoCandidateError{msg: "cursor chat store is absent"}
		}
		return coopNamedStoreCandidate{}, fmt.Errorf("scan Cursor chat store: %w", err)
	}
	return selectCoopNamedStoreCandidate(candidates, "Cursor chat")
}

func (cursorNamedStoreReader) readName(candidate coopNamedStoreCandidate) (string, error) {
	data, err := os.ReadFile(candidate.storePath)
	if err != nil {
		return "", err
	}
	var meta cursorChatMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("decode Cursor metadata %q: %w", candidate.storePath, err)
	}
	return meta.Title, nil
}

func cursorChatCreationTime(metaPath string, meta cursorChatMeta) (time.Time, bool, error) {
	for _, value := range []struct {
		field string
		raw   json.RawMessage
	}{
		{field: "createdAtMs", raw: meta.CreatedAtMs},
		{field: "createdAt", raw: meta.CreatedAt},
	} {
		if raw := bytes.TrimSpace(value.raw); len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		createdAt, err := parseCursorCreationValue(value.raw, value.field)
		if err != nil {
			return time.Time{}, false, err
		}
		return createdAt, true, nil
	}
	return cursorChatDirectoryBirthTime(metaPath)
}

func parseCursorCreationValue(raw json.RawMessage, field string) (time.Time, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) != 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return time.Time{}, fmt.Errorf("decode %s: %w", field, err)
		}
		if createdAt, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return createdAt, nil
		}
		millis, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse %s %q: %w", field, value, err)
		}
		return time.UnixMilli(millis), nil
	}
	var millis int64
	if err := json.Unmarshal(raw, &millis); err != nil {
		return time.Time{}, fmt.Errorf("decode %s: %w", field, err)
	}
	return time.UnixMilli(millis), nil
}
