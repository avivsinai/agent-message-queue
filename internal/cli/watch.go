package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type watchResult struct {
	Event    string    `json:"event"`
	Messages []msgInfo `json:"messages,omitempty"`
}

type msgInfo struct {
	ID         string   `json:"id"`
	From       string   `json:"from"`
	Subject    string   `json:"subject"`
	Thread     string   `json:"thread"`
	Created    string   `json:"created"`
	Path       string   `json:"path"`
	Priority   string   `json:"priority,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	ParseError string   `json:"parse_error,omitempty"`
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	common := addCommonFlags(fs)
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Maximum time to wait for messages (0 = wait forever)")
	pollFlag := fs.Bool("poll", false, "Use polling fallback instead of fsnotify (for network filesystems)")

	usage := usageWithFlags(fs, "amq watch --me <agent> [options]")
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

	inboxNew := fsq.AgentInboxNew(root, common.Me)

	// Ensure inbox directory exists
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// Set up context with timeout
	ctx := context.Background()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	// Watch for new messages (includes initial check after watcher setup)
	var messages []msgInfo
	var event string
	var watchErr error

	if *pollFlag {
		messages, event, watchErr = watchWithPolling(ctx, inboxNew, validator)
	} else {
		messages, event, watchErr = watchWithFsnotify(ctx, inboxNew, validator)
	}

	if watchErr != nil {
		if errors.Is(watchErr, context.DeadlineExceeded) {
			// Output timeout result but return a timeout exit code
			if err := outputWatchResult(common.JSON, "timeout", nil); err != nil {
				return err
			}
			return TimeoutError("watch timed out")
		}
		return watchErr
	}

	return outputWatchResult(common.JSON, event, messages)
}

func watchWithFsnotify(ctx context.Context, inboxNew string, validator *headerValidator) ([]msgInfo, string, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Fall back to polling if fsnotify fails
		return watchWithPolling(ctx, inboxNew, validator)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return watchWithPolling(ctx, inboxNew, validator)
	}

	// Check for existing messages AFTER watcher is set up to avoid race condition.
	// Any message arriving after this check will trigger a watcher event.
	existing, err := listNewMessages(inboxNew, validator)
	if err != nil {
		return nil, "", err
	}
	if len(existing) > 0 {
		return existing, "existing", nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil, "", errors.New("watcher closed")
			}
			// Only care about new files (Create or Rename into directory)
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				// Small delay to ensure file is fully written
				time.Sleep(10 * time.Millisecond)
				messages, err := listNewMessages(inboxNew, validator)
				if err != nil {
					return nil, "", err
				}
				if len(messages) > 0 {
					return messages, "new_message", nil
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil, "", errors.New("watcher closed")
			}
			return nil, "", err
		}
	}
}

func watchWithPolling(ctx context.Context, inboxNew string, validator *headerValidator) ([]msgInfo, string, error) {
	// Check for existing messages first
	existing, err := listNewMessages(inboxNew, validator)
	if err != nil {
		return nil, "", err
	}
	if len(existing) > 0 {
		return existing, "existing", nil
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-ticker.C:
			messages, err := listNewMessages(inboxNew, validator)
			if err != nil {
				return nil, "", err
			}
			if len(messages) > 0 {
				return messages, "new_message", nil
			}
		}
	}
}

func listNewMessages(inboxNew string, validator *headerValidator) ([]msgInfo, error) {
	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var messages []msgInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		// Skip dotfiles but process all .md files (even corrupt ones)
		if strings.HasPrefix(filename, ".") {
			continue
		}
		if !strings.HasSuffix(filename, ".md") {
			continue
		}

		path := filepath.Join(inboxNew, filename)
		baseID := strings.TrimSuffix(filename, ".md")
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			// Include corrupt messages so watch doesn't hang
			messages = append(messages, msgInfo{
				ID:         baseID,
				Path:       path,
				ParseError: err.Error(),
			})
			continue
		}
		if err := validator.validate(header); err != nil {
			parseErr := "invalid header: " + err.Error()
			id := baseID
			if safeID, ok := safeHeaderID(header.ID); ok {
				id = safeID
			}
			messages = append(messages, msgInfo{
				ID:         id,
				Path:       path,
				ParseError: parseErr,
			})
			continue
		}

		messages = append(messages, msgInfo{
			ID:       header.ID,
			From:     header.From,
			Subject:  header.Subject,
			Thread:   header.Thread,
			Created:  header.Created,
			Path:     path,
			Priority: header.Priority,
			Kind:     header.Kind,
			Labels:   header.Labels,
		})
	}

	// Sort by creation time
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Created < messages[j].Created
	})

	return messages, nil
}

func outputWatchResult(jsonOutput bool, event string, messages []msgInfo) error {
	result := watchResult{
		Event:    event,
		Messages: messages,
	}

	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	switch event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "existing":
		if err := writeStdoutLine("Found existing messages:"); err != nil {
			return err
		}
	case "new_message":
		if err := writeStdoutLine("New message(s) received:"); err != nil {
			return err
		}
	}

	for _, msg := range messages {
		subject := msg.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("  %s  %s  %s  %s\n", msg.Created, msg.From, msg.ID, subject); err != nil {
			return err
		}
	}

	return nil
}
