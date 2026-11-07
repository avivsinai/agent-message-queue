package fsq

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HistoryFile is one read message (inbox/cur), sent copy (outbox/sent) or
// receipt in an agent mailbox.
type HistoryFile struct {
	Path    string
	Agent   string
	Size    int64
	Created time.Time
}

// FindHistoryFiles returns, per agent mailbox under root, the history files
// older than cutoff and then the oldest of the rest until the remainder fits
// in maxBytes. A zero cutoff or maxBytes leaves that limit off. A file's age
// is the creation stamp that starts its message id, or its modification time
// when the name has none. Unread mail, dead letters and temporary files are
// never history, dead-letter receipts are the only record of a corrupt
// message and stay, remote files (which an unread message may still
// reference) are not scanned, and a message whose id is in keep stays with
// its receipts.
func FindHistoryFiles(root string, cutoff time.Time, maxBytes int64, keep map[string]bool) ([]HistoryFile, error) {
	agents, err := ListAgents(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []HistoryFile{}, nil
		}
		return nil, err
	}
	matches := []HistoryFile{}
	for _, agent := range agents {
		var files []HistoryFile
		var total int64
		for _, dir := range []string{AgentInboxCur(root, agent), AgentOutboxSent(root, agent), AgentReceipts(root, agent)} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			for _, entry := range entries {
				if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(entry.Name(), "__dlq.json") {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					continue // removed while scanning
				}
				if keep[historyID(entry.Name())] {
					total += info.Size()
					continue
				}
				files = append(files, HistoryFile{Path: filepath.Join(dir, entry.Name()), Agent: agent, Size: info.Size(), Created: historyStamp(entry.Name(), info.ModTime())})
				total += info.Size()
			}
		}
		sort.Slice(files, func(i, j int) bool {
			if !files[i].Created.Equal(files[j].Created) {
				return files[i].Created.Before(files[j].Created)
			}
			return files[i].Path < files[j].Path
		})
		for _, f := range files {
			old := !cutoff.IsZero() && f.Created.Before(cutoff)
			over := maxBytes > 0 && total > maxBytes
			if !old && !over {
				break
			}
			matches = append(matches, f)
			total -= f.Size
		}
	}
	return matches, nil
}

func historyStamp(name string, modified time.Time) time.Time {
	stamp, _, _ := strings.Cut(name, "_")
	if created, err := time.Parse("2006-01-02T15-04-05.000Z", stamp); err == nil {
		return created
	}
	return modified
}

// historyID is the message id a history file holds: the name of a message
// file without .md, or the id that starts a receipt name.
func historyID(name string) string {
	if id, _, ok := strings.Cut(name, "__"); ok {
		return id
	}
	return strings.TrimSuffix(name, ".md")
}
