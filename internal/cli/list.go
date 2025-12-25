package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type listItem struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Thread  string    `json:"thread"`
	Created string    `json:"created"`
	Box     string    `json:"box"`
	Path    string    `json:"path"`
	SortKey time.Time `json:"-"`
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List messages in inbox/new")
	curFlag := fs.Bool("cur", false, "List messages in inbox/cur")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	box := "new"
	if *newFlag && *curFlag {
		return errors.New("use only one of --new or --cur")
	}
	if *curFlag {
		box = "cur"
	}

	root := filepath.Clean(common.Root)
	var dir string
	if box == "new" {
		dir = fsq.AgentInboxNew(root, common.Me)
	} else {
		dir = fsq.AgentInboxCur(root, common.Me)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if common.JSON {
				return writeJSON(os.Stdout, []listItem{})
			}
			if err := writeStdoutLine("No messages."); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	items := make([]listItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if err := writeStderr("warning: skipping corrupt message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		item := listItem{
			ID:      header.ID,
			From:    header.From,
			Subject: header.Subject,
			Thread:  header.Thread,
			Created: header.Created,
			Box:     box,
			Path:    path,
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}

	if len(items) == 0 {
		if err := writeStdoutLine("No messages."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("%s  %s  %s  %s\n", item.Created, item.From, item.ID, strings.TrimSpace(subject)); err != nil {
			return err
		}
	}
	return nil
}
