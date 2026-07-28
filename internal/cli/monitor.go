package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type monitorResult struct {
	Event      string        `json:"event"`                 // "messages", "timeout", "empty"
	WatchEvent string        `json:"watch_event,omitempty"` // "existing", "new_message", ""
	Mode       string        `json:"mode,omitempty"`        // "drain", "peek"
	Session    string        `json:"session,omitempty"`     // session name, if inside a session
	Me         string        `json:"me"`
	Count      int           `json:"count"`
	Drained    []monitorItem `json:"drained"`
}

var (
	monitorIdleForTest        func()
	monitorPollingIdleForTest func()
)

func runMonitor(args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	common := addCommonFlags(fs)
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Max time to wait for messages (0 = wait forever)")
	pollFlag := fs.Bool("poll", false, "Use polling fallback instead of fsnotify")
	includeBodyFlag := fs.Bool("include-body", false, "Include message body in output")
	limitFlag := fs.Int("limit", 20, "Max messages to drain (0 = no limit)")
	peekFlag := fs.Bool("peek", false, "Peek without moving messages to cur")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq monitor --me <agent> [--session <name>] [options]",
		"Combined watch+drain: waits for messages, drains them, outputs structured payload.",
		"Use --peek to watch without moving messages to cur (no ack).",
		"Ideal for co-op mode background watchers in Claude Code or Codex.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *timeoutFlag < 0 {
		return UsageError("--timeout must be >= 0")
	}
	if *limitFlag < 0 {
		return UsageError("--limit must be >= 0")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("monitor", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}
	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	if err := requireMailboxDeliveryRoot(deliveryRoot, root, me); err != nil {
		return err
	}

	if err := validateKnownHandlesDeliveryRoot(deliveryRoot, common.Strict, me); err != nil {
		return err
	}
	validator, err := newHeaderValidatorDeliveryRoot(deliveryRoot, common.Strict)
	if err != nil {
		return err
	}

	inboxNew := filepath.Join("agents", common.Me, "inbox", "new")
	inboxNewDisplay := deliveryRoot.DisplayPath(inboxNew)
	revalidateContext := func() error {
		if err := guardMailboxContext("monitor", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
			return err
		}
		return deliveryRoot.VerifyBase()
	}

	session := resolveSessionName(root)

	mode := "drain"
	if *peekFlag {
		mode = "peek"
	}

	// First, try to drain existing messages
	items, err := monitorInboxItems(
		deliveryRoot,
		root,
		common.Me,
		*includeBodyFlag,
		*limitFlag,
		validator,
		mode,
		revalidateContext,
	)
	initialResult := monitorResult{
		Event:      "messages",
		WatchEvent: "existing",
		Mode:       mode,
		Session:    session,
		Me:         common.Me,
		Count:      len(items),
		Drained:    items,
	}
	if handled, finishErr := finishMonitorCollection(common.JSON, root, initialResult, err); handled {
		return finishErr
	}

	// No existing messages - wait for new ones
	ctx := context.Background()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	var watchEvent string
	var watchErr error

	if *pollFlag {
		watchEvent, watchErr = monitorWithPollingDeliveryRoot(ctx, deliveryRoot, inboxNew, revalidateContext)
	} else {
		watchEvent, watchErr = monitorWithFsnotifyDeliveryRoot(ctx, deliveryRoot, inboxNew, revalidateContext)
	}
	if err := revalidateContext(); err != nil {
		return err
	}

	if watchErr != nil {
		if os.IsNotExist(watchErr) {
			return NotFoundError("mailbox for %q disappeared while monitoring %s", common.Me, inboxNewDisplay)
		}
		if errors.Is(watchErr, context.DeadlineExceeded) {
			if err := outputMonitorResult(common.JSON, monitorResult{
				Event:   "timeout",
				Mode:    mode,
				Session: session,
				Me:      common.Me,
				Count:   0,
				Drained: []monitorItem{},
			}); err != nil {
				return err
			}
			return TimeoutError("monitor timed out")
		}
		return watchErr
	}

	// New message arrived - drain it
	if err := requireMailboxDeliveryRoot(deliveryRoot, root, me); err != nil {
		return err
	}
	items, err = monitorInboxItems(
		deliveryRoot,
		root,
		common.Me,
		*includeBodyFlag,
		*limitFlag,
		validator,
		mode,
		revalidateContext,
	)

	result := monitorResult{
		Event:      "messages",
		WatchEvent: watchEvent,
		Mode:       mode,
		Session:    session,
		Me:         common.Me,
		Count:      len(items),
		Drained:    items,
	}

	if handled, finishErr := finishMonitorCollection(common.JSON, root, result, err); handled {
		return finishErr
	}

	if len(items) == 0 {
		result.Event = "empty"
	}

	return outputMonitorResult(common.JSON, result)
}

func finishMonitorCollection(jsonOutput bool, root string, result monitorResult, collectErr error) (bool, error) {
	// A drain may have committed earlier claims before a later failure. Surface
	// those payloads first, then preserve the non-zero error for orchestration.
	if len(result.Drained) > 0 {
		if err := outputMonitorResult(jsonOutput, result); err != nil {
			if collectErr != nil {
				return true, errors.Join(collectErr, err)
			}
			return true, err
		}
	}
	if collectErr != nil {
		var committed *fsq.CommittedDurabilityError
		if errors.As(collectErr, &committed) {
			return true, collectErr
		}
		if os.IsNotExist(collectErr) {
			return true, NotFoundError("mailbox for %q disappeared while monitoring root %s", result.Me, root)
		}
		return true, collectErr
	}
	return len(result.Drained) > 0, nil
}

func monitorInboxItems(
	deliveryRoot *fsq.DeliveryRoot,
	root, me string,
	includeBody bool,
	limit int,
	validator *headerValidator,
	mode string,
	revalidateContext func() error,
) ([]monitorItem, error) {
	// A drain batch is one finite transaction: authorize immediately before it,
	// then finish every claim, parse, receipt, and payload in that batch. A
	// context change discovered mid-batch must not hide already-claimed messages
	// from the consumer. Idle monitor loops revalidate separately while waiting.
	if revalidateContext != nil {
		if err := revalidateContext(); err != nil {
			return nil, err
		}
	}
	if mode == "peek" {
		return collectInboxItems(deliveryRoot, me, includeBody, limit, validator, revalidateContext)
	}
	return drainInboxItems(deliveryRoot, root, me, includeBody, limit, validator)
}

func monitorWithFsnotify(ctx context.Context, inboxNew string, revalidateContext func() error) (string, error) {
	return monitorWithFsnotifyProbe(
		ctx,
		inboxNew,
		func() (bool, error) { return hasMessageFiles(inboxNew) },
		revalidateContext,
	)
}

func monitorWithFsnotifyDeliveryRoot(
	ctx context.Context,
	root *fsq.DeliveryRoot,
	inboxNew string,
	revalidateContext func() error,
) (string, error) {
	return monitorWithFsnotifyProbe(
		ctx,
		root.DisplayPath(inboxNew),
		func() (bool, error) { return hasMessageFilesDeliveryRoot(root, inboxNew) },
		revalidateContext,
	)
}

func monitorWithFsnotifyProbe(
	ctx context.Context,
	inboxNewDisplay string,
	hasMessages func() (bool, error),
	revalidateContext func() error,
) (string, error) {
	if err := revalidateContext(); err != nil {
		return "", err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return monitorWithPollingProbe(ctx, hasMessages, revalidateContext)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNewDisplay); err != nil {
		return monitorWithPollingProbe(ctx, hasMessages, revalidateContext)
	}

	// Check for existing messages AFTER setting up watcher to avoid race condition
	// (messages arriving between drain and watcher setup would be missed otherwise)
	if err := revalidateContext(); err != nil {
		return "", err
	}
	found, err := hasMessages()
	if err != nil {
		return "", err
	}
	if found {
		return "existing", nil
	}
	if monitorIdleForTest != nil {
		monitorIdleForTest()
		if err := revalidateContext(); err != nil {
			return "", err
		}
	}

	contextTicker := time.NewTicker(500 * time.Millisecond)
	defer contextTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-contextTicker.C:
			if err := revalidateContext(); err != nil {
				return "", err
			}
		case event, ok := <-watcher.Events:
			if !ok {
				return "", errors.New("watcher closed")
			}
			if err := revalidateContext(); err != nil {
				return "", err
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 &&
				filepath.Clean(event.Name) == filepath.Clean(inboxNewDisplay) {
				return "", os.ErrNotExist
			}
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				time.Sleep(10 * time.Millisecond)
				if err := revalidateContext(); err != nil {
					return "", err
				}
				return "new_message", nil
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return "", errors.New("watcher closed")
			}
			if contextErr := revalidateContext(); contextErr != nil {
				return "", contextErr
			}
			return "", err
		}
	}
}

func monitorWithPolling(ctx context.Context, inboxNew string, revalidateContext func() error) (string, error) {
	return monitorWithPollingProbe(
		ctx,
		func() (bool, error) { return hasMessageFiles(inboxNew) },
		revalidateContext,
	)
}

func monitorWithPollingDeliveryRoot(
	ctx context.Context,
	root *fsq.DeliveryRoot,
	inboxNew string,
	revalidateContext func() error,
) (string, error) {
	return monitorWithPollingProbe(
		ctx,
		func() (bool, error) { return hasMessageFilesDeliveryRoot(root, inboxNew) },
		revalidateContext,
	)
}

func monitorWithPollingProbe(
	ctx context.Context,
	hasMessages func() (bool, error),
	revalidateContext func() error,
) (string, error) {
	// Check immediately first to avoid missing messages that arrived before polling started
	if err := revalidateContext(); err != nil {
		return "", err
	}
	found, err := hasMessages()
	if err != nil {
		return "", err
	}
	if found {
		return "existing", nil
	}
	if monitorIdleForTest != nil {
		monitorIdleForTest()
		if err := revalidateContext(); err != nil {
			return "", err
		}
	}
	if monitorPollingIdleForTest != nil {
		monitorPollingIdleForTest()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if err := revalidateContext(); err != nil {
				return "", err
			}
			found, err := hasMessages()
			if err != nil {
				return "", err
			}
			if found {
				return "new_message", nil
			}
		}
	}
}

// hasMessageFiles checks if inbox/new contains any message files (.md, non-dotfile)
func hasMessageFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return messageFilesPresent(entries), nil
}

func hasMessageFilesDeliveryRoot(root *fsq.DeliveryRoot, dir string) (bool, error) {
	entries, err := root.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return messageFilesPresent(entries), nil
}

func messageFilesPresent(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip dotfiles (like .DS_Store) and require .md suffix
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".md") {
			return true
		}
	}
	return false
}

func outputMonitorResult(jsonOutput bool, result monitorResult) error {
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	mode := result.Mode
	if mode == "" {
		mode = "drain"
	}

	switch result.Event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "empty":
		if mode == "peek" {
			return writeStdoutLine("No messages available")
		}
		return writeStdoutLine("No messages to drain")
	case "messages":
		prefix := notificationPrefix("[AMQ]", result.Session)
		header := prefix + " %d message(s) for %s:\n\n"
		if mode == "peek" {
			header = prefix + " %d message(s) available for %s (peek):\n\n"
		}
		if err := writeStdout(header, result.Count, result.Me); err != nil {
			return err
		}
		for _, item := range result.Drained {
			// Handle corrupt/unparseable messages like drain does
			if item.ParseError != "" {
				dlqNote := ""
				if item.MovedToDLQ {
					dlqNote = " [moved to DLQ]"
				}
				if err := writeStdout("- ID: %s\n  ERROR: %s%s\n---\n", item.ID, item.ParseError, dlqNote); err != nil {
					return err
				}
				continue
			}
			subject := item.Subject
			if subject == "" {
				subject = "(no subject)"
			}
			priority := item.Priority
			if priority == "" {
				priority = "-"
			}
			kind := item.Kind
			if kind == "" {
				kind = "-"
			}
			if err := writeStdout("- From: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Thread: %s\n",
				item.From, item.ID, subject, priority, kind, item.Thread); err != nil {
				return err
			}
			if item.Body != "" {
				if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
					return err
				}
			}
			if err := writeStdout("---\n"); err != nil {
				return err
			}
		}
	}
	return nil
}
