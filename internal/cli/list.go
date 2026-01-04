package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type listItem struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
	Thread   string    `json:"thread"`
	Created  string    `json:"created"`
	Box      string    `json:"box"`
	Path     string    `json:"path"`
	Priority string    `json:"priority,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Labels   []string  `json:"labels,omitempty"`
	SortKey  time.Time `json:"-"`
}

func (l listItem) GetCreated() string {
	return l.Created
}

func (l listItem) GetID() string {
	return l.ID
}

func (l listItem) GetRawTime() time.Time {
	return l.SortKey
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List messages in inbox/new")
	curFlag := fs.Bool("cur", false, "List messages in inbox/cur")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")
	offsetFlag := fs.Int("offset", 0, "Offset into sorted results (0 = start)")

	// Filter flags
	priorityFlag := fs.String("priority", "", "Filter by priority (urgent, normal, low)")
	fromFlag := fs.String("from", "", "Filter by sender handle")
	kindFlag := fs.String("kind", "", "Filter by message kind")
	var labelFlags multiStringFlag
	fs.Var(&labelFlags, "label", "Filter by label (can be repeated)")

	usage := usageWithFlags(fs, "amq list --me <agent> [--new | --cur] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}
	validator, err := newHeaderValidator(root, common.Strict)
	if err != nil {
		return err
	}

	box := "new"
	if *newFlag && *curFlag {
		return UsageError("use only one of --new or --cur")
	}
	if *curFlag {
		box = "cur"
	}
	if *limitFlag < 0 {
		return UsageError("--limit must be >= 0")
	}
	if *offsetFlag < 0 {
		return UsageError("--offset must be >= 0")
	}

	// Validate filter values (allow empty, but reject invalid non-empty values)
	if *priorityFlag != "" && !format.IsValidPriority(*priorityFlag) {
		return UsageError("--priority must be one of: urgent, normal, low")
	}
	if *kindFlag != "" && !format.IsValidKind(*kindFlag) {
		return UsageError("--kind must be one of: brainstorm, review_request, review_response, question, answer, decision, status, todo")
	}

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
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if err := writeStderr("warning: skipping corrupt message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		if err := validator.validate(header); err != nil {
			if err := writeStderr("warning: skipping invalid message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		item := listItem{
			ID:       header.ID,
			From:     header.From,
			Subject:  header.Subject,
			Thread:   header.Thread,
			Created:  header.Created,
			Box:      box,
			Path:     path,
			Priority: header.Priority,
			Kind:     header.Kind,
			Labels:   header.Labels,
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
		}
		items = append(items, item)
	}

	// Apply filters
	filterOpts := FilterOptions{
		Priority: *priorityFlag,
		From:     *fromFlag,
		Kind:     *kindFlag,
		Labels:   labelFlags,
	}
	items = FilterMessages(items, filterOpts)

	format.SortByTimestamp(items)

	if *offsetFlag > 0 {
		if *offsetFlag >= len(items) {
			items = []listItem{}
		} else {
			items = items[*offsetFlag:]
		}
	}
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
	}

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
		priority := item.Priority
		if priority == "" {
			priority = "-"
		}
		if err := writeStdout("%s  %-6s  %s  %s  %s\n", item.Created, priority, item.From, item.ID, strings.TrimSpace(subject)); err != nil {
			return err
		}
	}
	return nil
}
