//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

// wakeLock represents the lock file content for wake process deduplication.
type wakeLock struct {
	PID     int    `json:"pid"`
	TTY     string `json:"tty"`
	Root    string `json:"root"`    // Absolute path to disambiguate relative AM_ROOT
	Started string `json:"started"` // ISO8601 timestamp
}

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox.
// Returns cleanup function and error. If another wake is running, returns error.
func acquireWakeLock(root, me string) (cleanup func(), err error) {
	agentBase := fsq.AgentBase(root, me)
	lockPath := filepath.Join(agentBase, ".wake.lock")

	// Ensure agent directory exists before attempting lock
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create agent directory: %w", err)
	}

	// Resolve absolute path for comparison
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}

	// Check existing lock
	if data, err := os.ReadFile(lockPath); err == nil {
		var existing wakeLock
		if json.Unmarshal(data, &existing) == nil {
			// Check if process is still alive
			if processAlive(existing.PID) {
				// Same inbox (compare absolute paths)?
				if existing.Root == absRoot {
					return nil, fmt.Errorf("wake already running for %s (pid %d on %s since %s)",
						me, existing.PID, existing.TTY, existing.Started)
				}
			}
			// Stale lock - remove it before trying to acquire
			_ = os.Remove(lockPath)
		}
	}

	// Get TTY name
	ttyName := "unknown"
	if link, err := os.Readlink("/dev/fd/0"); err == nil {
		ttyName = link
	} else if fi, err := os.Stdin.Stat(); err == nil {
		ttyName = fi.Name()
	}

	// Write lock atomically using O_EXCL to prevent race conditions
	lock := wakeLock{
		PID:     os.Getpid(),
		TTY:     ttyName,
		Root:    absRoot,
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	lockData, _ := json.Marshal(lock)

	// Use O_EXCL for atomic creation - fails if file exists (race protection)
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the race - re-read to get its info
			if data, readErr := os.ReadFile(lockPath); readErr == nil {
				var winner wakeLock
				if json.Unmarshal(data, &winner) == nil {
					return nil, fmt.Errorf("wake already running for %s (pid %d on %s since %s)",
						me, winner.PID, winner.TTY, winner.Started)
				}
			}
			return nil, fmt.Errorf("wake lock exists for %s (concurrent start)", me)
		}
		return nil, fmt.Errorf("failed to create wake lock: %w", err)
	}
	_, writeErr := f.Write(lockData)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("failed to write wake lock: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("failed to close wake lock: %w", closeErr)
	}

	cleanup = func() {
		// Only remove if it's still our lock
		if data, err := os.ReadFile(lockPath); err == nil {
			var current wakeLock
			if json.Unmarshal(data, &current) == nil && current.PID == os.Getpid() {
				_ = os.Remove(lockPath)
			}
		}
	}

	return cleanup, nil
}

// processAlive checks if a process with given PID is running.
func processAlive(pid int) bool {
	// Guard against invalid PIDs - pid<=0 would signal process group
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; send signal 0 to check
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func runWake(args []string) error {
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	common := addCommonFlags(fs)
	injectCmdFlag := fs.String("inject-cmd", "", "Command to inject (power user mode)")
	bellFlag := fs.Bool("bell", false, "Ring terminal bell on new messages")
	debounceFlag := fs.Duration("debounce", 250*time.Millisecond, "Debounce window for batching messages")
	previewLenFlag := fs.Int("preview-len", 48, "Max subject preview length")
	injectModeFlag := fs.String("inject-mode", "auto", "Injection mode: auto, raw, paste (auto detects CLI type)")

	usage := usageWithFlags(fs, "amq wake --me <agent> [options]",
		"Background waker: injects terminal notification when messages arrive.",
		"Run as background job before starting CLI: amq wake --me claude &",
		"",
		"Inject modes:",
		"  auto  - Detect CLI type: raw for Claude Code/Codex, paste for others",
		"  raw   - Plain text + CR, no bracketed paste (works with Ink-based CLIs)",
		"  paste - Bracketed paste with delayed CR (works with crossterm-based CLIs)",
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

	// Acquire lock to prevent duplicate wake processes
	cleanup, err := acquireWakeLock(root, me)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg := wakeConfig{
		me:           me,
		root:         root,
		injectCmd:    *injectCmdFlag,
		bell:         *bellFlag,
		debounce:     *debounceFlag,
		previewLen:   *previewLenFlag,
		strict:       common.Strict,
		fallbackWarn: true,
		injectMode:   *injectModeFlag,
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

	// Ignore job control signals so background job can operate freely:
	// - SIGTTOU: allow writing to TTY from background
	// - SIGTSTP: prevent Ctrl+Z or shell from suspending us
	// - SIGTTIN: prevent suspension if stdin is accidentally read
	signal.Ignore(syscall.SIGTTOU, syscall.SIGTSTP, syscall.SIGTTIN)

	// Handle signals gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Debounce timer
	var debounceTimer *time.Timer
	pendingNotify := false

	// TTY health check timer - verify we can still inject every 30s
	healthTicker := time.NewTicker(30 * time.Second)
	defer healthTicker.Stop()

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

		case <-healthTicker.C:
			// Verify TTY is still valid by checking if we can open /dev/tty
			if !tiocsti.IsTTY() {
				return errors.New("TTY no longer available")
			}
		}
	}
}
