package adapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const (
	codexQueueName               = "codex-queue"
	codexQueueTargetThreadPrefix = "codex-queue:thread:"
	// Live-verified success line from `codex queue` (codex-cli 0.149.1):
	// `Queued message <id> for thread <uuid>.`
	codexQueueSuccessMarker = "Queued message "
)

// CodexQueue implements the honest submitted seat into a live Codex GUI/TUI
// thread. `codex queue --thread <uuid> --message <text>` calls the app-server
// JSON-RPC `thread/queue/add`; the EXISTING writer (GUI app-server / TUI /
// codex-acp) drains it. The adapter never raises or focuses the app
// (ActivationNone) and never claims that the turn finished — exit 0 proves
// enqueue to the writer, not completion.
//
// Queue cannot create a thread, so the only target is
// `codex-queue:thread:<uuid>`. An idle thread (no holder of
// thread-writer-locks/<uuid>.lock) is ErrTargetDegraded; deferred delivery for
// that case is out of scope.
type CodexQueue struct {
	Runner            CommandRunner
	LookPath          func(string) (string, error)
	InspectWriterLock WriterLockInspector
	Home              string
}

func (CodexQueue) Name() string { return codexQueueName }

// Capability is the honest submitted vector. Queue enqueues to the thread's
// existing writer; it never raises/focuses the app.
func (CodexQueue) Capability() Capability {
	return Capability{
		Activation:    ActivationNone,
		Delivery:      DeliverySubmitted,
		Session:       SessionExistingExact,
		RequiresHuman: false,
	}
}

func (c CodexQueue) CapabilityForTarget(target string) (Capability, error) {
	if _, err := parseCodexQueueThreadUUID(target); err != nil {
		return Capability{}, err
	}
	return c.Capability(), nil
}

func (CodexQueue) NormalizeTarget(target string) (string, error) {
	id, err := parseCodexQueueThreadUUID(target)
	if err != nil {
		return "", err
	}
	return codexQueueTargetThreadPrefix + id, nil
}

func (c CodexQueue) Probe(ctx context.Context, target string) error {
	_, _, err := c.probe(ctx, target)
	return err
}

func (c CodexQueue) Inject(ctx context.Context, target string, payload string) error {
	uuid, codexPath, err := c.probe(ctx, target)
	if err != nil {
		return fmt.Errorf("inject Codex queue: %w", err)
	}
	out, err := c.runner().Run(ctx, codexPath, "queue", "--thread", uuid, "--message", payload)
	if err != nil {
		// Live `codex queue` (0.149.1): non-zero with
		// `Error: failed to queue session message: thread/queue/add failed: ...`
		// means the enqueue did not land — replayable. Uncertain (do not replay)
		// only when that same non-zero exit still prints the verified success
		// marker `Queued message `.
		if strings.Contains(string(out), codexQueueSuccessMarker) {
			return fmt.Errorf("%w: %s", ErrInjectUncertain, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("inject Codex queue: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c CodexQueue) probe(ctx context.Context, target string) (uuid, codexPath string, err error) {
	normalized, err := c.NormalizeTarget(target)
	if err != nil {
		return "", "", err
	}
	codexPath, err = c.pinExecutable(ctx)
	if err != nil {
		return "", "", err
	}
	uuid, err = parseCodexQueueThreadUUID(normalized)
	if err != nil {
		return "", "", err
	}
	home, err := c.codexHome()
	if err != nil {
		return "", "", err
	}
	n, err := countCodexRollouts(home, uuid)
	if err != nil {
		return "", "", err
	}
	if n != 1 {
		return "", "", fmt.Errorf("%w: want exactly one rollout for thread %s under %s, found %d", ErrTargetNotFound, uuid, home, n)
	}
	lockPath := filepath.Join(home, "thread-writer-locks", uuid+".lock")
	held, err := c.lockInspector().Held(ctx, lockPath)
	if err != nil {
		return "", "", fmt.Errorf("inspect writer lock for thread %s: %w; open it in the Codex app or `codex resume %s` and retry", uuid, err, uuid)
	}
	if !held {
		return "", "", fmt.Errorf("%w: thread %s has no active writer; open it in the Codex app or `codex resume %s` and retry", ErrTargetDegraded, uuid, uuid)
	}
	return uuid, codexPath, nil
}

func (c CodexQueue) pinExecutable(ctx context.Context) (string, error) {
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	looked, err := lookPath("codex")
	if err != nil {
		return "", fmt.Errorf("codex not found on PATH (%w); put a codex >= 0.149 first in PATH", err)
	}
	if !filepath.IsAbs(looked) {
		abs, absErr := filepath.Abs(looked)
		if absErr != nil {
			return "", fmt.Errorf("resolve codex executable: %w; put a codex >= 0.149 first in PATH", absErr)
		}
		looked = abs
	}
	// Mirror launch.ProviderForExecutable: identity is the PATH basename
	// before symlink evaluation so version-manager shims keep the name `codex`.
	if launch.ProviderForExecutable(looked) != launch.CodexProvider {
		return "", fmt.Errorf("codex at %s has basename %q, want %q; put a codex >= 0.149 first in PATH", looked, filepath.Base(looked), launch.CodexProvider)
	}
	resolved, err := filepath.EvalSymlinks(looked)
	if err != nil {
		return "", fmt.Errorf("resolve codex at %s: %w; put a codex >= 0.149 first in PATH", looked, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat codex at %s: %w; put a codex >= 0.149 first in PATH", resolved, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("codex at %s is not an executable regular file; put a codex >= 0.149 first in PATH", resolved)
	}
	out, err := c.runner().Run(ctx, resolved, "queue", "--help")
	help := string(out)
	if err != nil || !strings.Contains(help, "--thread") || !strings.Contains(help, "--message") {
		return "", fmt.Errorf("codex at %s lacks `queue`; put a codex >= 0.149 first in PATH", resolved)
	}
	return resolved, nil
}

func (c CodexQueue) codexHome() (string, error) {
	if strings.TrimSpace(c.Home) != "" {
		return c.Home, nil
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func (c CodexQueue) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c CodexQueue) lockInspector() WriterLockInspector {
	if c.InspectWriterLock != nil {
		return c.InspectWriterLock
	}
	return platformWriterLockInspector{}
}

func parseCodexQueueThreadUUID(target string) (string, error) {
	id, ok := strings.CutPrefix(strings.TrimSpace(target), codexQueueTargetThreadPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported Codex queue target %q; use %s<uuid>", target, codexQueueTargetThreadPrefix)
	}
	id = strings.TrimSpace(id)
	if !lowercaseThreadUUIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid Codex queue thread uuid %q; want an exact lowercase 8-4-4-4-12 hex uuid", id)
	}
	return id, nil
}

func countCodexRollouts(home, uuid string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+uuid+".jsonl"))
	if err != nil {
		return 0, err
	}
	return len(matches), nil
}
