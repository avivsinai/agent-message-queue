package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type wakeConfig struct {
	me           string
	root         string
	injectCmd    string
	bell         bool
	debounce     time.Duration
	previewLen   int
	strict       bool
	fallbackWarn bool
}

func runWake(args []string) error {
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	common := addCommonFlags(fs)
	injectCmdFlag := fs.String("inject-cmd", "", "Command to inject (power user mode)")
	bellFlag := fs.Bool("bell", false, "Ring terminal bell on new messages")
	debounceFlag := fs.Duration("debounce", 250*time.Millisecond, "Debounce window for batching messages")
	previewLenFlag := fs.Int("preview-len", 48, "Max subject preview length")

	usage := usageWithFlags(fs, "amq wake --me <agent> [options]",
		"Background waker: injects terminal notification when messages arrive.",
		"Run as background job before starting CLI: amq wake --me claude &",
		"",
		"EXPERIMENTAL: Uses TIOCSTI ioctl (macOS/Linux). May not work on all systems.")
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

	root := filepath.Clean(common.Root)
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	// Verify TIOCSTI is available
	if !tiocsti.Available() {
		return errors.New("TIOCSTI not available on this platform; use tmux send-keys or terminal-specific injection")
	}

	// Verify we have a real TTY
	if !tiocsti.IsTTY() {
		return errors.New("amq wake requires a real terminal (run in foreground or as background job in same terminal)")
	}

	cfg := wakeConfig{
		me:           me,
		root:         root,
		injectCmd:    *injectCmdFlag,
		bell:         *bellFlag,
		debounce:     *debounceFlag,
		previewLen:   *previewLenFlag,
		strict:       common.Strict,
		fallbackWarn: true,
	}

	return runWakeLoop(cfg)
}

func runWakeLoop(cfg wakeConfig) error {
	inboxNew := fsq.AgentInboxNew(cfg.root, cfg.me)

	// Ensure inbox exists
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// Set up watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return fmt.Errorf("failed to watch inbox: %w", err)
	}

	// Ignore SIGTTOU so background job can write to TTY
	signal.Ignore(syscall.SIGTTOU)

	// Handle signals gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Debounce timer
	var debounceTimer *time.Timer
	pendingNotify := false

	// Notify if messages already exist
	if err := notifyNewMessages(&cfg); err != nil {
		_ = writeStderr("amq wake: notify error: %v\n", err)
	}

	for {
		var debounceC <-chan time.Time
		if debounceTimer != nil {
			debounceC = debounceTimer.C
		}

		select {
		case <-sigCh:
			// Clean exit on SIGHUP/SIGTERM
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			// Only care about new files
			if event.Op&(fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			// Skip non-.md files
			if !strings.HasSuffix(event.Name, ".md") {
				continue
			}

			// Start or reset debounce timer
			pendingNotify = true
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(cfg.debounce)
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
			}
			debounceTimer.Reset(cfg.debounce)

		case err, ok := <-watcher.Errors:
			if !ok {
				return errors.New("watcher closed")
			}
			_ = writeStderr("amq wake: watcher error: %v\n", err)

		case <-debounceC:
			if !pendingNotify {
				continue
			}
			pendingNotify = false

			// Collect and notify
			if err := notifyNewMessages(&cfg); err != nil {
				_ = writeStderr("amq wake: notify error: %v\n", err)
			}
		}
	}
}

type wakeMsgInfo struct {
	from    string
	subject string
}

func notifyNewMessages(cfg *wakeConfig) error {
	inboxNew := fsq.AgentInboxNew(cfg.root, cfg.me)

	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var messages []wakeMsgInfo
	senderCounts := make(map[string]int)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}

		path := filepath.Join(inboxNew, name)
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Count corrupt messages too
			messages = append(messages, wakeMsgInfo{from: "unknown", subject: "(parse error)"})
			senderCounts["unknown"]++
			continue
		}

		from := strings.TrimSpace(header.From)
		if from == "" {
			from = "unknown"
		}
		subject := strings.TrimSpace(header.Subject)
		subject = sanitizeForTTY(subject)
		from = sanitizeForTTY(from)

		messages = append(messages, wakeMsgInfo{
			from:    from,
			subject: subject,
		})
		senderCounts[from]++
	}

	if len(messages) == 0 {
		return nil
	}

	// Build notification text
	var text string

	if cfg.injectCmd != "" {
		// Power user mode: inject actual command
		text = "\n" + cfg.injectCmd + "\n"
	} else {
		// Default: informational notice
		text = buildNotificationText(messages, senderCounts, cfg.previewLen)
	}

	// Keep plain text for stderr fallback
	plainText := text
	if cfg.bell {
		plainText = "\a" + plainText
	}

	// Wrap in bracketed paste escape sequences so TUI apps (crossterm/ratatui)
	// treat it as a paste event rather than individual keystrokes
	// Start: ESC[200~  End: ESC[201~
	// Include \r inside paste for Ink-based apps (Claude Code)
	// Also send \r after for crossterm-based apps (Codex)
	injectedText := "\x1b[200~" + text + "\r\x1b[201~\r"
	if cfg.bell {
		injectedText = "\a" + injectedText
	}

	// Inject into terminal
	if err := tiocsti.Inject(injectedText); err != nil {
		if cfg.fallbackWarn {
			_ = writeStderr("amq wake: TIOCSTI injection failed: %v\n", err)
			_ = writeStderr("amq wake: falling back to stderr notification\n")
			cfg.fallbackWarn = false
		}
		// Fallback: print plain text to stderr (no escape sequences)
		_, _ = fmt.Fprint(os.Stderr, plainText+"\n")
		return nil
	}

	return nil
}

func buildNotificationText(messages []wakeMsgInfo, senderCounts map[string]int, previewLen int) string {
	count := len(messages)

	if count == 1 {
		// Single message: show from + truncated subject
		msg := messages[0]
		subject := msg.subject
		if subject == "" {
			subject = "(no subject)"
		}
		subject = truncateSubject(subject, previewLen)
		return fmt.Sprintf("AMQ: message from %s - %s. Run: amq drain --include-body", msg.from, subject)
	}

	// Multiple messages: show counts by sender
	var parts []string
	senders := make([]string, 0, len(senderCounts))
	for s := range senderCounts {
		senders = append(senders, s)
	}
	sort.Strings(senders)

	for _, sender := range senders {
		c := senderCounts[sender]
		parts = append(parts, fmt.Sprintf("%d from %s", c, sender))
	}

	return fmt.Sprintf("AMQ: %d messages - %s. Run: amq drain --include-body",
		count, strings.Join(parts, ", "))
}

func truncateSubject(subject string, previewLen int) string {
	if previewLen <= 0 {
		return ""
	}
	runes := []rune(subject)
	if len(runes) <= previewLen {
		return subject
	}
	if previewLen <= 3 {
		return string(runes[:previewLen])
	}
	return string(runes[:previewLen-3]) + "..."
}

func sanitizeForTTY(s string) string {
	return strings.Map(func(r rune) rune {
		// Filter ASCII controls (0x00-0x1F), DEL (0x7F), and C1 controls (0x80-0x9F)
		// C1 range includes 0x9B which some terminals interpret as CSI
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}
