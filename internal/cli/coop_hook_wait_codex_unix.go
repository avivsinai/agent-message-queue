//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/writerlock"
)

// The Codex waiter is the doorbell of a joined Codex chat. amq coop
// install-hooks --harness codex runs it as an async hook: Codex keeps it in
// the background for the thread and kills it at its timeout or when the
// thread unloads. It rings by queueing the doorbell to the thread with
// codex queue, which starts a turn in an idle chat; the turn's Stop hook
// then starts the next waiter.

// envCodexAppCLI names the Codex binary of the desktop app in the
// environment the app gives its hooks.
const envCodexAppCLI = "CODEX_APP_RUNTIME_CLI_PATH"

// codexWaitRunner runs codex for the Codex waiter's ring; nil runs it.
var codexWaitRunner adapter.CommandRunner

// runCoopWaitCodex is the Codex waiter. The hook input's session_id is the
// thread. It returns nil, and so exits 0, in every case: when it is not
// this chat's doorbell (bad input, no binding or one that cannot be used,
// no mailbox) and after a ring.
func runCoopWaitCodex(ctx context.Context, stdin io.Reader) error {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if json.NewDecoder(io.LimitReader(stdin, maxWakeDialogHookInputBytes)).Decode(&in) != nil || !codexidentity.ValidThread(in.SessionID) {
		return nil
	}
	thread := in.SessionID
	explicit := false
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			explicit = true
		}
	}
	if adoptCodexHookIdentity(thread) != nil {
		return nil
	}
	root, me, _, err := coopHookContext()
	if err != nil || root == "" || me == "" {
		return nil
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return nil
	}
	codexHome = absPath(filepath.Clean(codexHome))
	if info, err := os.Stat(fsq.AgentBase(root, me)); err != nil || !info.IsDir() {
		return nil
	}
	held := func() bool { return true }
	if !explicit {
		held = func() bool { return codexBindingStillHeld(thread, codexHome, root, me) }
	}
	return waitCoopMail(ctx, canonicalWakeRoot(root), me, codexWaiter(ctx, thread, codexHome, held))
}

// codexWaiter is the waiter of a Codex thread under codexHome.
func codexWaiter(ctx context.Context, thread, codexHome string, held func() bool) coopWaiter {
	lockPath := codexWriterLockPath(codexHome, thread)
	return coopWaiter{
		session: thread,
		parent:  os.Getppid(),
		held:    held,
		// Codex holds the lock while the thread is loaded; an unknown
		// state is not proof that it unloaded.
		gone: func() bool {
			loaded, err := writerlock.Held(ctx, lockPath)
			return err == nil && !loaded
		},
		ring: func() (bool, error) {
			err := ringCodexThread(ctx, thread, codexHome)
			if err == nil || errors.Is(err, adapter.ErrInjectUncertain) {
				return false, nil
			}
			// Codex shows a Stop hook's systemMessage at the next turn.
			out, merr := json.Marshal(struct {
				SystemMessage string `json:"systemMessage"`
			}{fmt.Sprintf("AMQ could not ring this chat for new mail: %s. Run amq drain --include-body.", strings.TrimSuffix(err.Error(), "."))})
			if merr == nil {
				_ = writeStdout("%s\n", out)
			}
			return true, nil
		},
	}
}

// ringCodexThread queues the doorbell to thread. The Codex binary is the
// desktop app's when its hook environment names one, with the same checks
// as codex on PATH; a named binary that fails them is a failed ring, not a
// reason to run another.
func ringCodexThread(ctx context.Context, thread, codexHome string) error {
	queue := adapter.CodexQueue{Runner: codexWaitRunner, Home: codexHome}
	if app, ok := os.LookupEnv(envCodexAppCLI); ok {
		if !filepath.IsAbs(app) {
			return fmt.Errorf("%s=%q is not an absolute path", envCodexAppCLI, app)
		}
		queue.LookPath = func(string) (string, error) { return app, nil }
	}
	return queue.Inject(ctx, "codex-queue:thread:"+thread, harness.Doorbell)
}

// codexBindingStillHeld reports whether thread under codexHome still
// carries the identity (root, me) its waiter adopted.
func codexBindingStillHeld(thread, codexHome, root, me string) bool {
	rec, err := codexidentity.Lookup(thread, codexHome)
	return err == nil && rec.Me == me && canonicalWakeRoot(rec.Root) == canonicalWakeRoot(root)
}
