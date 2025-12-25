package thread

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Entry is a thread message entry.
type Entry struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      []string  `json:"to"`
	Thread  string    `json:"thread"`
	Subject string    `json:"subject"`
	Created string    `json:"created"`
	Body    string    `json:"body,omitempty"`
	RawTime time.Time `json:"-"`
}

// Collect scans agent mailboxes and returns messages for a thread.
// onError is called when a message cannot be parsed; returning a non-nil error aborts the scan.
func Collect(root, threadID string, agents []string, includeBody bool, onError func(path string, err error) error) ([]Entry, error) {
	entries := []Entry{}
	seen := make(map[string]struct{})
	for _, agent := range agents {
		dirs := []string{
			fsq.AgentInboxNew(root, agent),
			fsq.AgentInboxCur(root, agent),
			fsq.AgentOutboxSent(root, agent),
		}
		for _, dir := range dirs {
			files, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			for _, file := range files {
				if file.IsDir() {
					continue
				}
				path := filepath.Join(dir, file.Name())
				if includeBody {
					msg, err := format.ReadMessageFile(path)
					if err != nil {
						if onError != nil {
							if cbErr := onError(path, err); cbErr != nil {
								return nil, cbErr
							}
						}
						continue
					}
					if msg.Header.Thread != threadID {
						continue
					}
					if _, ok := seen[msg.Header.ID]; ok {
						continue
					}
					seen[msg.Header.ID] = struct{}{}
					entry := Entry{
						ID:      msg.Header.ID,
						From:    msg.Header.From,
						To:      msg.Header.To,
						Thread:  msg.Header.Thread,
						Subject: msg.Header.Subject,
						Created: msg.Header.Created,
						Body:    msg.Body,
					}
					if ts, err := time.Parse(time.RFC3339Nano, msg.Header.Created); err == nil {
						entry.RawTime = ts
					}
					entries = append(entries, entry)
					continue
				}

				header, err := format.ReadHeaderFile(path)
				if err != nil {
					if onError != nil {
						if cbErr := onError(path, err); cbErr != nil {
							return nil, cbErr
						}
					}
					continue
				}
				if header.Thread != threadID {
					continue
				}
				if _, ok := seen[header.ID]; ok {
					continue
				}
				seen[header.ID] = struct{}{}
				entry := Entry{
					ID:      header.ID,
					From:    header.From,
					To:      header.To,
					Thread:  header.Thread,
					Subject: header.Subject,
					Created: header.Created,
				}
				if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
					entry.RawTime = ts
				}
				entries = append(entries, entry)
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].RawTime.IsZero() && !entries[j].RawTime.IsZero() {
			if entries[i].RawTime.Equal(entries[j].RawTime) {
				return entries[i].ID < entries[j].ID
			}
			return entries[i].RawTime.Before(entries[j].RawTime)
		}
		if entries[i].Created == entries[j].Created {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Created < entries[j].Created
	})

	return entries, nil
}
