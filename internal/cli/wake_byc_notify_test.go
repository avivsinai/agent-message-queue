//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
