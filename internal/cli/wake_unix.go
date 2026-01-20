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
	"golang.org/x/sys/unix"
)

// wakeLock represents the lock file content for wake process deduplication.
type wakeLock struct {
	PID     int    `json:"pid"`
	TTY     string `json:"tty"`
	Root    string `json:"root"`    // Absolute path to disambiguate relative AM_ROOT
	Started string `json:"started"` // ISO8601 timestamp
}

// sanitizeTTYForFilename converts a TTY path to a safe filename component.
// e.g., "/dev/ttys001" -> "ttys001", "/dev/pts/1" -> "pts-1"
func sanitizeTTYForFilename(tty string) string {
	if tty == "" || tty == "unknown" {
		return "unknown"
	}
	// Remove /dev/ prefix
	name := strings.TrimPrefix(tty, "/dev/")
	// Replace path separators with dashes
	name = strings.ReplaceAll(name, "/", "-")
	// Remove any other unsafe characters
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	if name == "" {
		return "unknown"
	}
	return name
}

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox on this TTY.
// Returns cleanup function and error. If another wake is running on THIS TTY, returns error.
// Different TTYs can run independent wake processes for the same agent.
func acquireWakeLock(root, me string) (cleanup func(), err error) {
	agentBase := fsq.AgentBase(root, me)

	// Get current TTY early - needed for lock filename
	currentTTY := getCurrentTTY()
	ttySuffix := sanitizeTTYForFilename(currentTTY)
	lockPath := filepath.Join(agentBase, fmt.Sprintf(".wake.%s.lock", ttySuffix))

	// Ensure agent directory exists before attempting lock
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create agent directory: %w", err)
	}

	// Resolve absolute path for comparison (normalize symlinks if possible)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = realRoot
	}

	// Migration: handle legacy .wake.lock from previous versions.
	// Old versions used a single lock file; new versions use per-TTY locks.
	// If a legacy wake is running on our TTY, kill it to prevent duplicates.
	legacyLockPath := filepath.Join(agentBase, ".wake.lock")
	if data, err := os.ReadFile(legacyLockPath); err == nil {
		var legacy wakeLock
		if json.Unmarshal(data, &legacy) == nil && processAlive(legacy.PID) {
			// Legacy wake is running. Check if it's on our TTY.
			legacyTTY := legacy.TTY
			if strings.HasPrefix(legacyTTY, "/dev/") {
				if real, err := filepath.EvalSymlinks(legacyTTY); err == nil {
					legacyTTY = real
				}
			}
			if currentTTY != "" && currentTTY == legacyTTY {
				// Same TTY - kill the legacy process to prevent duplicate notifications
				if proc, err := os.FindProcess(legacy.PID); err == nil {
					_ = proc.Signal(syscall.SIGTERM)
					time.Sleep(100 * time.Millisecond)
					if processAlive(legacy.PID) {
						_ = proc.Signal(syscall.SIGKILL)
					}
				}
			}
		}
		// Always remove legacy lock (dead process, different TTY, or just killed)
		_ = os.Remove(legacyLockPath)
	}

	// Check existing per-TTY lock
	if data, err := os.ReadFile(lockPath); err == nil {
		var existing wakeLock
		if json.Unmarshal(data, &existing) == nil {
			// Normalize stored root if possible to avoid symlink mismatches.
			if realRoot, err := filepath.EvalSymlinks(existing.Root); err == nil {
				existing.Root = realRoot
			}

			// Check if process is still alive
			if processAlive(existing.PID) {
				// Process alive, but check if its TTY is still valid.
				// If terminal was closed, the wake process is orphaned and should be replaced.
				// Only check absolute paths (skip "stdin", "unknown", "pipe:[...]", etc.)
				if strings.HasPrefix(existing.TTY, "/dev/") {
					if _, statErr := os.Stat(existing.TTY); os.IsNotExist(statErr) {
						// TTY gone - orphaned wake, kill it and take over
						if proc, err := os.FindProcess(existing.PID); err == nil {
							_ = proc.Signal(syscall.SIGTERM)
							time.Sleep(100 * time.Millisecond)
							if processAlive(existing.PID) {
								_ = proc.Signal(syscall.SIGKILL)
							}
						}
						_ = os.Remove(lockPath)
						goto createLock
					}
				}

				// Check if existing wake is in a different session (orphaned from closed shell).
				// Same TTY + different session = old wake is orphaned, safe to take over.
				existingTTY := existing.TTY
				if strings.HasPrefix(existingTTY, "/dev/") {
					if real, err := filepath.EvalSymlinks(existingTTY); err == nil {
						existingTTY = real
					}
				}
				if currentTTY != "" && currentTTY == existingTTY {
					// Same TTY - check session IDs
					existingSid, sidErr := unix.Getsid(existing.PID)
					currentSid, _ := unix.Getsid(0)
					if sidErr == nil && existingSid != currentSid {
						// Different session on same TTY - old one is orphaned.
						// Kill the orphaned process before taking over to prevent duplicates.
						if proc, err := os.FindProcess(existing.PID); err == nil {
							_ = proc.Signal(syscall.SIGTERM)
							// Brief wait for graceful shutdown
							time.Sleep(100 * time.Millisecond)
							// Force kill if still alive
							if processAlive(existing.PID) {
								_ = proc.Signal(syscall.SIGKILL)
							}
						}
						_ = os.Remove(lockPath)
						goto createLock
					}
				}

				return nil, fmt.Errorf("wake already running for %s (pid %d on %s since %s)",
					me, existing.PID, existing.TTY, existing.Started)
			}
			// Stale lock - remove it before trying to acquire
			_ = os.Remove(lockPath)
		} else {
			// Invalid/corrupt lock. If it was just created, avoid stomping a writer in progress.
			if info, statErr := os.Stat(lockPath); statErr == nil {
				if time.Since(info.ModTime()) < 2*time.Second {
					return nil, fmt.Errorf("wake lock is being created (retry shortly)")
				}
			}
			_ = os.Remove(lockPath)
		}
	}

createLock:
	// Use currentTTY obtained at function start (already normalized)
	ttyName := currentTTY
	if ttyName == "" {
		ttyName = "unknown"
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
	// On Unix, FindProcess always succeeds; send signal 0 to check.
	// ESRCH => process doesn't exist (dead).
	// EPERM => process exists but we lack permission (alive).
	// nil   => process exists and we can signal it (alive).
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true // process exists, just can't signal it
	}
	return false // ESRCH or other error => treat as dead
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

	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
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

	// Ignore job control signals so background job can operate freely.
	// Note: This also affects foreground mode (Ctrl+Z won't suspend), but wake
	// is designed to run as a background job (amq wake &) so this is intentional.
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
			if !ttyAvailable() {
				return errors.New("TTY no longer available")
			}
		}
	}
}

func ttyAvailable() bool {
	// Mirrors injection path: if /dev/tty can't be opened, wake can't inject.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// getCurrentTTY returns the normalized path to the current controlling terminal.
func getCurrentTTY() string {
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = tty.Close() }()
	if link, err := os.Readlink(fmt.Sprintf("/dev/fd/%d", tty.Fd())); err == nil {
		// Normalize symlinks for reliable comparison
		if real, err := filepath.EvalSymlinks(link); err == nil {
			return real
		}
		return link
	}
	return ""
}
