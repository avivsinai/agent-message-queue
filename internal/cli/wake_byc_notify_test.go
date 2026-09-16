//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// TestBycNotifyNewMessagesRefusesDetachedInbox (byc-b) proves the retained
// inbox is revalidated for canonical identity before each authoritative read.
//
// Before the fix, notifyNewMessages called cfg.retainedInbox.ReadDir() WITHOUT
// calling ValidateCanonical first. A directory swap after admission left the
// retained FD pointing at the detached OLD inbox; messages landing in the
// canonical inbox were never seen. SILENT NON-DELIVERY.
//
// The fix calls ValidateCanonical (via interface assertion) before ReadDir in
// notifyNewMessages. A swap renames the canonical inbox/new away and replaces
// it; the retained FD still points at the OLD inode, so SameFile fails and we
// refuse rather than reading a detached namespace.
//
// Mutation RED: remove the ValidateCanonical call in notifyNewMessages -> the
// swap goes undetected, ReadDir reads the detached OLD inbox (empty), returns
// nil, and the test's refusal assertion fails.
func TestBycNotifyNewMessagesRefusesDetachedInbox(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inboxDir.Close() }()

	cfg := &wakeConfig{
		root:                    root,
		me:                      "codex",
		retainedInbox:           inboxDir,
		retainedInboxRebindable: false,
	}

	// Sanity: before the swap, notifyNewMessages reads the canonical inbox
	// (empty) without error.
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("pre-swap notifyNewMessages: %v", err)
	}

	// Swap the canonical inbox/new: rename the OLD directory away and replace
	// it with a fresh directory at the same path. The retained FD still points
	// at the OLD (now-detached) inode.
	inboxNew := filepath.Join(root, "agents", "codex", "inbox", "new")
	detached := inboxNew + ".detached-byc"
	if err := os.Rename(inboxNew, detached); err != nil {
		t.Fatalf("swap: rename canonical inbox/new away: %v", err)
	}
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		t.Fatalf("swap: mkdir replacement inbox/new: %v", err)
	}

	// notifyNewMessages must REFUSE: the retained inbox no longer matches the
	// canonical namespace. It must NOT silently read the detached OLD inbox.
	//
	// codex #788: the refusal must route through the CALLER correctly. The
	// caller (wake_unix.go runWakeLoop) checks errors.As(err, &scanErr) FIRST:
	// if the swap error is a *wakeInboxScanError, the caller schedules an
	// ordinary scan retry and returns nil — looping forever on the same
	// detached inbox. The swap error must be a DEDICATED
	// *wakeInboxCanonicalMismatchError (distinct from *wakeInboxScanError) so
	// the caller's scanErr check does NOT match and it falls through to
	// classifyWakeFailure -> wakeFailureFatal (terminal exit / re-admission),
	// mirroring rebindWatcher's handling. Exercise the caller's actual
	// routing, not just that notifyNewMessages returned non-nil.
	err = notifyNewMessages(cfg)
	if err == nil {
		t.Fatal("notifyNewMessages read the detached inbox without error (byc: must revalidate canonical identity before ReadDir and refuse on a swap)")
	}
	// (1) Must NOT match *wakeInboxScanError — otherwise the caller's ordinary
	// scan-retry loop keeps re-validating the same detached inbox forever.
	var scanErr *wakeInboxScanError
	if errors.As(err, &scanErr) {
		t.Fatalf("notifyNewMessages swap error must NOT be *wakeInboxScanError (codex #788): the caller would route it to ordinary scan retry and loop on the detached inbox; got %T: %v", err, err)
	}
	// (2) Must be a dedicated *wakeInboxCanonicalMismatchError so
	// classifyWakeFailure routes it to wakeFailureFatal (terminal exit), not
	// ordinary retry.
	var mismatch *wakeInboxCanonicalMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("notifyNewMessages swap error must be *wakeInboxCanonicalMismatchError (codex #788) so the caller exits instead of ordinary scan-retry; got %T: %v", err, err)
	}
	if got := classifyWakeFailure(err); got != wakeFailureFatal {
		t.Fatalf("classifyWakeFailure(swap error) = %v, want wakeFailureFatal (byc: canonical authority loss must terminate, not loop in ordinary scan retry)", got)
	}
}

// TestBycCallerRoutingFatalExitOnDetachedRetainedInbox (byc-b, codex #788)
// exercises the CALLER's dispatch routing (runWakeLoop wake_unix.go:3745-3759),
// not just notifyNewMessages returning an error. The caller checks
// errors.As(err, &scanErr) FIRST: a *wakeInboxScanError routes to ordinary scan
// retry (loops on the same detached inbox). A *wakeInboxCanonicalMismatchError
// must bypass that check and fall through to classifyWakeFailure -> fatal exit.
//
// This replicates the caller's exact two-step decision and asserts the swap
// error routes to TERMINAL EXIT, not ordinary retry. Mutation RED: return
// *wakeInboxScanError instead -> the caller's errors.As(scanErr) matches ->
// ordinary retry loop -> the fatal-exit assertion fails.
func TestBycCallerRoutingFatalExitOnDetachedRetainedInbox(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inboxDir.Close() }()
	cfg := &wakeConfig{
		root:                    root,
		me:                      "codex",
		retainedInbox:           inboxDir,
		retainedInboxRebindable: false,
	}

	// Swap the canonical inbox/new (rename old away, mkdir replacement). The
	// retained FD still points at the OLD (now-detached) inode.
	inboxNew := filepath.Join(root, "agents", "codex", "inbox", "new")
	detached := inboxNew + ".detached-byc-caller"
	if err := os.Rename(inboxNew, detached); err != nil {
		t.Fatalf("swap: rename canonical inbox/new away: %v", err)
	}
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		t.Fatalf("swap: mkdir replacement inbox/new: %v", err)
	}

	notifyErr := notifyNewMessages(cfg)
	if notifyErr == nil {
		t.Fatal("notifyNewMessages read the detached inbox without error")
	}

	// Replicate the caller's exact dispatch (runWakeLoop wake_unix.go:3745-3759).
	// The caller does errors.As(err, &scanErr) FIRST; if it matches, ordinary
	// scan retry (loop). Otherwise classifyWakeFailure -> fatal exit.
	var scanErr *wakeInboxScanError
	if errors.As(notifyErr, &scanErr) {
		t.Fatalf("caller would route swap error to ORDINARY SCAN RETRY (loop on detached inbox) — codex #788: errors.As(scanErr) matched; got %T: %v", notifyErr, notifyErr)
	}
	// Fell through to classifyWakeFailure — must be fatal (terminal exit).
	if got := classifyWakeFailure(notifyErr); got != wakeFailureFatal {
		t.Fatalf("caller classifyWakeFailure(swap error) = %v, want wakeFailureFatal (byc: canonical authority loss must terminate, not loop in ordinary scan retry)", got)
	}
}

// TestBycRebindableCallerReadmitsOnCanonicalMismatch (byc-b, codex #788 r3)
// is an INTEGRATION smoke test of the ordinary/rebindable caller path: the
// wake loop admits with an ordinary (rebindable) retained inbox; the canonical
// inbox/new is swapped (rename old away, mkdir replacement); a message is
// delivered to the NEW canonical inbox. The loop must re-arm and emit
// attention for the message that lives only in the NEW canonical inbox.
//
// NOTE: in the live loop a directory rename also fires an independent watcher
// event that re-arms via retryWatcher, so this integration test can be masked
// by that independent rearm. The PRECISE codex #788 rebindable routing
// (canonical mismatch -> invalidate watcher -> rebindWatcher re-arms) is proven
// by TestBycClassifyCanonicalMismatch, the unit test of the extracted routing
// decision. Both tests together cover the rebindable path.
func TestBycRebindableCallerReadmitsOnCanonicalMismatch(t *testing.T) {
	originalScanRetryBase := wakeInboxScanRetryBase
	originalScanRetryMax := wakeInboxScanRetryMax
	wakeInboxScanRetryBase = 20 * time.Millisecond
	wakeInboxScanRetryMax = 100 * time.Millisecond
	t.Cleanup(func() {
		wakeInboxScanRetryBase = originalScanRetryBase
		wakeInboxScanRetryMax = originalScanRetryMax
	})

	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	inboxPath := fsq.AgentInboxNew(root, "codex")
	ready := make(chan struct{})
	attention := make(chan string, 8)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWakeLoop(wakeConfig{
			root:        root,
			me:          "codex",
			session:     "session1",
			wakeOwner:   &wakeOwner{},
			debounce:    5 * time.Millisecond,
			previewLen:  80,
			injectMode:  wakeInjectModeNone,
			controlStop: stop,
			onPrepared: func(wakeAdmissionWatcher) error {
				close(ready)
				return nil
			},
			preconditionCheck: func(*wakeConfig) error { return nil },
			attentionIsTTY:    func() bool { return false },
			attentionWrite: func(data []byte) (int, error) {
				attention <- string(data)
				return len(data), nil
			},
		})
	}()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("wake loop did not stop")
		}
	})

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("wake loop exited before readiness: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("wake loop did not publish readiness")
	}

	// Swap the canonical inbox/new: the retained FD still points at the OLD
	// (now-detached) inode. Deliver a message to the NEW canonical inbox.
	detached := inboxPath + ".detached-byc-rebind"
	if err := os.Rename(inboxPath, detached); err != nil {
		t.Fatalf("swap: rename canonical inbox/new away: %v", err)
	}
	if err := os.Mkdir(inboxPath, 0o700); err != nil {
		t.Fatalf("swap: mkdir replacement inbox/new: %v", err)
	}
	// Drop a message ONLY in the NEW canonical inbox. If the loop is stuck on
	// the detached OLD inbox, this message is never seen.
	deliverWakeWatcherMessageForTest(t, root, "codex", "byc-rebind-readmit", "readmit")

	// The loop must re-arm on the NEW canonical inbox and emit attention for
	// the message that lives only there. This is the codex #788 rebindable
	// re-admission assertion: the new canonical inbox is REACHED, not looped
	// past on the detached one.
	awaitWakeAttentionFrom(t, attention, done, "readmit")
}

// TestBycClassifyCanonicalMismatch (byc-b, codex #788 r3) is the PRECISE unit
// test of the rebindable routing decision extracted from attemptNotification.
// notifyNewMessages returns *wakeInboxCanonicalMismatchError for canonical
// authority loss (both rebindable and !rebindable). The caller routes it via
// classifyCanonicalMismatch:
//   - rebindable    -> canonicalMismatchRebindableRearm (invalidate watcher +
//     retained inbox; rebindWatcher re-arms from the canonical path on the next
//     inbox-scan-retry tick; ordinary retry, NOT fatal, NOT a loop on the
//     detached inbox).
//   - !rebindable   -> canonicalMismatchFatal (terminal exit, mirroring
//     rebindWatcher's retained-authority handling).
//   - non-mismatch  -> canonicalMismatchNone (fall through to ordinary scan
//     retry / classifyWakeFailure).
//
// Mutation RED: make classifyCanonicalMismatch return canonicalMismatchNone for
// the rebindable case (skip watcher invalidation) -> the caller falls through to
// errors.As(scanErr) which does NOT match (distinct type) -> classifyWakeFailure
// -> wakeFailureFatal -> terminal exit instead of rearm. The rebindable-rearm
// assertion fails.
func TestBycClassifyCanonicalMismatch(t *testing.T) {
	mismatchErr := &wakeInboxCanonicalMismatchError{
		detachedPath: "/tmp/inbox/new",
		cause:        errors.New("retained wake inbox directory no longer matches component authority"),
	}
	// A transient ReadDir error must NOT be classified as a canonical mismatch.
	scanErr := &wakeInboxScanError{err: errors.New("read inbox: transient EIO")}

	for _, tc := range []struct {
		name       string
		err        error
		rebindable bool
		want       canonicalMismatchDisposition
	}{
		{"rebindable canonical mismatch -> rearm", mismatchErr, true, canonicalMismatchRebindableRearm},
		{"!rebindable canonical mismatch -> fatal", mismatchErr, false, canonicalMismatchFatal},
		{"transient scan error -> none (ordinary retry)", scanErr, true, canonicalMismatchNone},
		{"transient scan error -> none (!rebindable)", scanErr, false, canonicalMismatchNone},
		{"nil error -> none", nil, true, canonicalMismatchNone},
		{"wrapped canonical mismatch -> rearm", fmt.Errorf("notify: %w", mismatchErr), true, canonicalMismatchRebindableRearm},
		{"wrapped canonical mismatch -> fatal", fmt.Errorf("notify: %w", mismatchErr), false, canonicalMismatchFatal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCanonicalMismatch(tc.err, tc.rebindable)
			if got != tc.want {
				t.Fatalf("classifyCanonicalMismatch(rebindable=%v) = %v, want %v", tc.rebindable, got, tc.want)
			}
		})
	}

	// Rebindable rearm must NOT be fatal: the whole point is that a legitimate
	// remove/recreate re-arms instead of terminating.
	if got := classifyCanonicalMismatch(mismatchErr, true); got == canonicalMismatchFatal {
		t.Fatalf("rebindable canonical mismatch must NOT be fatal (codex #788): a legitimate remove/recreate re-arms; got %v", got)
	}
	// !Rebindable must be fatal: a caller-provided retained inbox cannot be
	// rebound, so terminal exit mirrors rebindWatcher.
	if got := classifyCanonicalMismatch(mismatchErr, false); got != canonicalMismatchFatal {
		t.Fatalf("!rebindable canonical mismatch must be fatal (codex #788): caller-provided retained inbox cannot rebound; got %v", got)
	}
}
