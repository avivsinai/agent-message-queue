package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeConfig struct {
	me                string
	root              string
	injectCmd         string
	bell              bool
	debounce          time.Duration
	previewLen        int
	strict            bool
	fallbackWarn      bool
	injectMode        string // auto, raw, paste
	interrupt         bool
	interruptLabel    string
	interruptPriority string
	interruptKey      string
	interruptNotice   string
	interruptCooldown time.Duration
	lastInterrupt     time.Time
}

type wakeMsgInfo struct {
	from     string
	subject  string
	priority string
	labels   []string
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
	var interruptMessages []wakeMsgInfo
	interruptCounts := make(map[string]int)

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
		priority := strings.TrimSpace(header.Priority)

		info := wakeMsgInfo{
			from:     from,
			subject:  subject,
			priority: priority,
			labels:   header.Labels,
		}

		messages = append(messages, info)
		senderCounts[from]++

		if cfg.interrupt && isInterruptMessage(info, cfg) {
			interruptMessages = append(interruptMessages, info)
			interruptCounts[from]++
		}
	}

	if len(messages) == 0 {
		return nil
	}

	if cfg.interrupt && len(interruptMessages) > 0 {
		interruptText := buildInterruptText(interruptMessages, interruptCounts, cfg.previewLen, cfg.interruptNotice)
		now := time.Now()
		if cfg.interruptKey != "" && shouldInterruptNow(cfg, now) {
			if err := tiocsti.Inject(cfg.interruptKey); err == nil {
				cfg.lastInterrupt = now
				time.Sleep(50 * time.Millisecond)
			}
		}
		return injectNotification(cfg, interruptText)
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

	return injectNotification(cfg, text)
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
		return fmt.Sprintf("AMQ: message from %s - %s. Drain with: amq drain --include-body — then act on it", msg.from, subject)
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

	return fmt.Sprintf("AMQ: %d messages - %s. Drain with: amq drain --include-body — then act on it",
		count, strings.Join(parts, ", "))
}

func buildInterruptText(messages []wakeMsgInfo, senderCounts map[string]int, previewLen int, custom string) string {
	if custom != "" {
		return custom
	}

	count := len(messages)
	if count == 1 {
		msg := messages[0]
		subject := msg.subject
		if subject == "" {
			subject = "(no subject)"
		}
		subject = truncateSubject(subject, previewLen)
		return fmt.Sprintf("AMQ interrupt: urgent message from %s - %s. Drain with: amq drain --include-body — then act on it",
			msg.from, subject)
	}

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
	return fmt.Sprintf("AMQ interrupt: %d urgent messages - %s. Drain with: amq drain --include-body — then act on it",
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

func isInterruptMessage(info wakeMsgInfo, cfg *wakeConfig) bool {
	if !cfg.interrupt {
		return false
	}
	if cfg.interruptPriority != "" && info.priority != cfg.interruptPriority {
		return false
	}
	if cfg.interruptLabel == "" {
		return false
	}
	for _, label := range info.labels {
		if strings.TrimSpace(label) == cfg.interruptLabel {
			return true
		}
	}
	return false
}

func shouldInterruptNow(cfg *wakeConfig, now time.Time) bool {
	if cfg.interruptCooldown <= 0 {
		return true
	}
	return now.Sub(cfg.lastInterrupt) >= cfg.interruptCooldown
}

func effectiveInjectMode(cfg *wakeConfig) string {
	mode := cfg.injectMode
	if mode == "" || mode == "auto" {
		// Auto-detect: use raw mode for Claude Code and Codex to avoid bracketed-paste
		// Enter swallowing in some CLIs. Paste mode remains available via flag.
		// Claude Code's Ink framework has buggy bracketed paste handling where CR gets
		// coalesced with the paste-end sequence and swallowed by the input parser.
		meLower := strings.ToLower(cfg.me)
		if strings.Contains(meLower, "claude") || strings.Contains(meLower, "codex") {
			mode = "raw"
		} else {
			mode = "paste"
		}
	}
	return mode
}

func injectNotification(cfg *wakeConfig, text string) error {
	// Keep plain text for stderr fallback
	plainText := text
	if cfg.bell {
		plainText = "\a" + plainText
	}

	mode := effectiveInjectMode(cfg)
	var injectErr error
	switch mode {
	case "raw":
		// Raw mode: inject text and CR separately to avoid paste detection
		// Ink treats multi-char input as paste, not keypresses. Sending text+CR
		// as one chunk makes Ink see pasted text, not an Enter keypress.
		// Solution: inject text, wait, then inject CR as separate single byte.
		injectedText := text
		if cfg.bell {
			injectedText = "\a" + injectedText
		}
		if err := tiocsti.Inject(injectedText); err != nil {
			injectErr = err
		} else {
			// Delay so CR arrives in separate read cycle, detected as keypress
			time.Sleep(30 * time.Millisecond)
			injectErr = tiocsti.Inject("\r")
		}

	case "paste":
		// Paste mode: bracketed paste with delayed CR
		// Works with crossterm/ratatui apps
		// Send paste content first, then CR after short delay to avoid coalescing
		pasteText := "\x1b[200~" + text + "\x1b[201~"
		if cfg.bell {
			pasteText = "\a" + pasteText
		}
		if err := tiocsti.Inject(pasteText); err != nil {
			injectErr = err
		} else {
			// Small delay to ensure CR lands in separate read cycle
			time.Sleep(25 * time.Millisecond)
			injectErr = tiocsti.Inject("\r")
		}

	default:
		// Unknown mode, fall back to raw
		injectedText := text + "\r"
		if cfg.bell {
			injectedText = "\a" + injectedText
		}
		injectErr = tiocsti.Inject(injectedText)
	}

	if injectErr != nil {
		if cfg.fallbackWarn {
			_ = writeStderr("amq wake: TIOCSTI injection failed: %v\n", injectErr)
			_ = writeStderr("amq wake: falling back to stderr notification\n")
			cfg.fallbackWarn = false
		}
		// Fallback: print plain text to stderr (no escape sequences)
		_, _ = fmt.Fprint(os.Stderr, plainText+"\n")
		return nil
	}

	return nil
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
