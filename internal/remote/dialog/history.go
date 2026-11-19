package dialog

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// HistoryEntry holds the session, tool, wait and outcome of one resolved dialog.
type HistoryEntry struct {
	ID       string    `json:"id"`
	Binding  string    `json:"binding"`
	Harness  string    `json:"harness,omitempty"`
	Tool     string    `json:"tool,omitempty"`
	OpenedAt time.Time `json:"opened_at,omitzero"`
	At       time.Time `json:"resolved_at"`
	Outcome  string    `json:"outcome"`
	By       string    `json:"by"`
}

// historyMaxBytes bounds one history file; the log is at most two files.
const historyMaxBytes = 1 << 20

const (
	currentHistory  = "history.jsonl"
	previousHistory = "history.1.jsonl"
)

// AppendHistory makes one line durable before the dialog worker removes its
// source. The worker is the only writer.
func AppendHistory(e HistoryEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if len(line) > historyMaxBytes {
		return fmt.Errorf("dialog history entry exceeds %d bytes", historyMaxBytes)
	}
	dir, err := Root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, currentHistory)
	// Check the last byte so a partial append cannot absorb the next entry.
	existing, info, err := fsq.OpenRegularNoFollow(path)
	flags := os.O_RDWR | os.O_APPEND
	switch {
	case errors.Is(err, os.ErrNotExist):
		flags |= os.O_CREATE | os.O_EXCL
	case err != nil:
		return err
	default:
		var tail [1]byte
		if info.Size() > 0 {
			_, err = existing.ReadAt(tail[:], info.Size()-1)
		}
		if closeErr := existing.Close(); err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		partial := info.Size() > 0 && tail[0] != '\n'
		available := int64(historyMaxBytes) - info.Size()
		if int64(len(line)) > available || partial && int64(len(line)) == available {
			if err := os.Rename(path, filepath.Join(dir, previousHistory)); err != nil {
				return err
			}
			flags |= os.O_CREATE | os.O_EXCL
		} else if partial {
			line = append([]byte{'\n'}, line...)
		}
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if flags&os.O_CREATE == 0 {
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) {
			_ = f.Close()
			return fmt.Errorf("dialog history changed before append: %s", path)
		}
	}
	if _, err = f.Write(line); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return fsq.SyncDir(dir)
}

// History reads the retained resolutions from since onwards, oldest first.
// A retry after an append may have left another line for the same dialog.
func History(since time.Time) ([]HistoryEntry, error) {
	dir, err := Root()
	if err != nil {
		return nil, err
	}
	// Pin the current generation before reading the older one. A rotation
	// between the reads can expose a duplicate, but cannot skip that generation.
	current, info, err := fsq.OpenRegularNoFollowBounded(filepath.Join(dir, currentHistory), historyMaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if current != nil {
		defer func() { _ = current.Close() }()
	}
	older, err := fsq.ReadRegularNoFollowBounded(filepath.Join(dir, previousHistory), historyMaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var newer []byte
	if current != nil {
		newer, err = io.ReadAll(io.NewSectionReader(current, 0, info.Size()))
		if err != nil {
			return nil, err
		}
	}
	entries := []HistoryEntry{}
	seen := map[string]bool{}
	for _, data := range [][]byte{older, newer} {
		for line := range bytes.Lines(data) {
			var e HistoryEntry
			if json.Unmarshal(line, &e) != nil || e.ID == "" || e.Binding == "" || e.At.IsZero() || e.At.Before(since) || seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			entries = append(entries, e)
		}
	}
	slices.SortFunc(entries, func(a, b HistoryEntry) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.ID, b.ID))
	})
	return entries, nil
}
