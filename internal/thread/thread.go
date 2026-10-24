package thread

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Entry is a thread message entry.
type Entry struct {
	ID       string   `json:"id"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Thread   string   `json:"thread"`
	Subject  string   `json:"subject"`
	Created  string   `json:"created"`
	Body     string   `json:"body,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	// Refs are the message's reply references (the ids it answers). Not
	// part of the JSON output; consumers such as the ACP bridge correlate on
	// them.
	Refs    []string  `json:"-"`
	RawTime time.Time `json:"-"`
	// Routing and scope fields, also not part of the JSON output.
	ReplyTo      string `json:"-"`
	ReplyProject string `json:"-"`
	FromProject  string `json:"-"`
	// Occurrences lists every copy of the message that a scan saw. Only
	// CollectAll fills it.
	Occurrences []Occurrence `json:"-"`
}

// Occurrence is one copy of a message in a mailbox directory.
type Occurrence struct {
	Session string          // session of the queue root that holds the copy; empty for a base root
	Agent   string          // agent handle that owns the mailbox
	Dir     fsq.MailboxLeaf // inbox/new, inbox/cur, or outbox/sent
	Path    string
}

// ScanRoot names one queue root for CollectAll and ReadByID. Session is
// empty for a base root.
type ScanRoot struct {
	Session string
	Root    string
}

// ScanResult is the result of CollectAll.
type ScanResult struct {
	// Entries holds one record per message id, ordered by created then id.
	Entries []Entry
	// Pruned counts, per session, the files skipped unread because their
	// name stamp is older than the cutoff.
	Pruned map[string]int
}

func entryFromHeader(header format.Header) Entry {
	entry := Entry{
		ID:           header.ID,
		Refs:         header.Refs,
		From:         header.From,
		To:           header.To,
		Thread:       header.Thread,
		Subject:      header.Subject,
		Created:      header.Created,
		Priority:     header.Priority,
		Kind:         header.Kind,
		Labels:       header.Labels,
		ReplyTo:      header.ReplyTo,
		ReplyProject: header.ReplyProject,
		FromProject:  header.FromProject,
	}
	if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
		entry.RawTime = ts
	}
	return entry
}

func (e Entry) GetCreated() string {
	return e.Created
}

func (e Entry) GetID() string {
	return e.ID
}

func (e Entry) GetRawTime() time.Time {
	return e.RawTime
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
				name := file.Name()
				if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
					continue
				}
				path := filepath.Join(dir, name)
				if includeBody {
					msg, err := format.ReadMessageFile(path)
					if err != nil {
						if onError == nil {
							return nil, fmt.Errorf("parse message %s: %w", path, err)
						}
						if cbErr := onError(path, err); cbErr != nil {
							return nil, cbErr
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
					entry := entryFromHeader(msg.Header)
					entry.Body = msg.Body
					entries = append(entries, entry)
					continue
				}

				header, err := format.ReadHeaderFile(path)
				if err != nil {
					if onError == nil {
						return nil, fmt.Errorf("parse message %s: %w", path, err)
					}
					if cbErr := onError(path, err); cbErr != nil {
						return nil, cbErr
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
				entry := entryFromHeader(header)
				entries = append(entries, entry)
			}
		}
	}

	format.SortByTimestamp(entries)

	return entries, nil
}

// idStampLayout is the UTC creation stamp that starts every AMQ message id,
// and so every message file name (<id>.md).
const idStampLayout = "2006-01-02T15-04-05.000Z"

// CollectAll reads the header of every message under roots, in every thread
// and every agent mailbox, and returns one record per message id with every
// copy it saw. A file whose name starts with an id stamp older than cutoff is
// counted in Pruned and never opened; a zero cutoff reads every file. Bodies
// are never decoded. onError is called for a file whose header cannot be
// read; returning a non-nil error aborts the scan.
func CollectAll(roots []ScanRoot, cutoff time.Time, onError func(path string, err error) error) (ScanResult, error) {
	result := ScanResult{Entries: []Entry{}, Pruned: map[string]int{}}
	byID := make(map[string]int)
	leaves := []fsq.MailboxLeaf{fsq.MailboxInboxNew, fsq.MailboxInboxCur, fsq.MailboxOutboxSent}
	for _, scanRoot := range roots {
		agents, err := fsq.ListAgents(scanRoot.Root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ScanResult{}, err
		}
		for _, agent := range agents {
			for _, leaf := range leaves {
				dir := fsq.AgentMailboxPath(scanRoot.Root, agent, leaf)
				files, err := os.ReadDir(dir)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return ScanResult{}, err
				}
				for _, file := range files {
					name := file.Name()
					if file.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
						continue
					}
					if !cutoff.IsZero() && len(name) >= len(idStampLayout) {
						if stamp, err := time.Parse(idStampLayout, name[:len(idStampLayout)]); err == nil && stamp.Before(cutoff) {
							result.Pruned[scanRoot.Session]++
							continue
						}
					}
					path := filepath.Join(dir, name)
					occurrence := Occurrence{Session: scanRoot.Session, Agent: agent, Dir: leaf, Path: path}
					// Every copy of a message is named <id>.md, so a copy of an
					// id already read is recorded without opening it again.
					if idx, ok := byID[strings.TrimSuffix(name, ".md")]; ok {
						result.Entries[idx].Occurrences = append(result.Entries[idx].Occurrences, occurrence)
						continue
					}
					header, err := format.ReadHeaderFile(path)
					if err != nil {
						if onError == nil {
							return ScanResult{}, fmt.Errorf("parse message %s: %w", path, err)
						}
						if cbErr := onError(path, err); cbErr != nil {
							return ScanResult{}, cbErr
						}
						continue
					}
					if idx, ok := byID[header.ID]; ok {
						result.Entries[idx].Occurrences = append(result.Entries[idx].Occurrences, occurrence)
						continue
					}
					entry := entryFromHeader(header)
					entry.Occurrences = []Occurrence{occurrence}
					byID[header.ID] = len(result.Entries)
					result.Entries = append(result.Entries, entry)
				}
			}
		}
	}
	format.SortByTimestamp(result.Entries)
	return result, nil
}

// ReadByID reads the header of message id by its file name, <id>.md, from
// the inbox and sent directories of agents in each root. It lists no
// directory and ignores the cutoff. It returns one record with every copy it
// found; ok is false when no readable copy exists. An invalid id or handle
// is never joined into a path.
func ReadByID(roots []ScanRoot, agents []string, id string) (Entry, bool) {
	name := id + ".md"
	if fsq.ValidateMessageFilename(name) != nil {
		return Entry{}, false
	}
	leaves := []fsq.MailboxLeaf{fsq.MailboxInboxNew, fsq.MailboxInboxCur, fsq.MailboxOutboxSent}
	var entry Entry
	found := false
	for _, scanRoot := range roots {
		for _, agent := range agents {
			if fsq.ValidateHandle(agent) != nil {
				continue
			}
			for _, leaf := range leaves {
				path := filepath.Join(fsq.AgentMailboxPath(scanRoot.Root, agent, leaf), name)
				occurrence := Occurrence{Session: scanRoot.Session, Agent: agent, Dir: leaf, Path: path}
				if found {
					if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
						entry.Occurrences = append(entry.Occurrences, occurrence)
					}
					continue
				}
				header, err := format.ReadHeaderFile(path)
				if err != nil || header.ID != id {
					continue
				}
				entry = entryFromHeader(header)
				entry.Occurrences = []Occurrence{occurrence}
				found = true
			}
		}
	}
	return entry, found
}
