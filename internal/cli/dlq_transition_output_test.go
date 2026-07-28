package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestRunReadReportsOrdinaryDLQPlacementFailure(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "ordinary-read"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	blockDLQTmpWithRegularFile(t, root, "alice")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
	})
	if err == nil || GetExitCode(err) != ExitError {
		t.Fatalf("read error = %T %v (exit %d), want ordinary non-zero error", err, err, GetExitCode(err))
	}
	for _, want := range []string{
		"failed to parse message " + id,
		"deliver to dlq",
		filepath.Join("agents", "alice", "dlq", "tmp"),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("read error = %q, want detail %q", err, want)
		}
	}
	var partial *fsq.DLQTransitionError
	var committed *fsq.CommittedDurabilityError
	if errors.As(err, &partial) || errors.As(err, &committed) {
		t.Fatalf("read error = %T %v, want ordinary pre-envelope failure", err, err)
	}
	var item inboxItem
	if decodeErr := unmarshalJSONOutput(stdout, &item); decodeErr != nil {
		t.Fatalf("unmarshal read output: %v (output: %s)", decodeErr, stdout)
	}
	if item.ID != id || item.ParseError == "" || item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("ordinary pre-envelope read item = %#v", item)
	}
	assertOrdinaryDLQFailureState(t, root, id)
}

func TestRunReadOutputsCommittedClaimWhenDLQPlacementFailsBeforeEnvelope(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "committed-read-pre-envelope"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	blockDLQTmpWithRegularFile(t, root, "alice")
	claimErr := installReadCommittedClaimOnDLQPlacementFailure(t, id+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
	})

	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, claimErr) || GetExitCode(err) != ExitError {
		t.Fatalf("read error = %T %v (exit %d), want committed claim plus placement failure", err, err, GetExitCode(err))
	}
	wantCur := filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")
	if committed.FinalPath != wantCur || committed.Recipient != "alice" {
		t.Fatalf("committed claim metadata = %#v, want cur path %q", committed, wantCur)
	}
	for _, want := range []string{"deliver to dlq", filepath.Join("agents", "alice", "dlq", "tmp")} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("read error = %q, want placement detail %q", err, want)
		}
	}
	var item inboxItem
	if decodeErr := unmarshalJSONOutput(stdout, &item); decodeErr != nil {
		t.Fatalf("unmarshal read output: %v (output: %s)", decodeErr, stdout)
	}
	if item.ID != id || item.ParseError == "" || item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("committed pre-envelope read item = %#v", item)
	}
	assertOrdinaryDLQFailureState(t, root, id)
}

func TestRunReadOutputsRetainedSourceWhenDLQRemovalFails(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "partial-read"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	injectedErr := installReadDLQSourceRemovalFailure(t, root, "alice", id+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
	})

	var transition *fsq.DLQTransitionError
	if !errors.As(err, &transition) || !errors.Is(err, injectedErr) || GetExitCode(err) != ExitError {
		t.Fatalf("read error = %T %v, want partial DLQ transition error", err, err)
	}
	wantSource := filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")
	wantEnvelope := filepath.Join(fsq.AgentDLQNew(root, "alice"), "partial-"+id+".md")
	if !transition.SourceRetained || transition.SourcePath != wantSource || transition.EnvelopePath != wantEnvelope {
		t.Fatalf("partial transition metadata = %#v", transition)
	}
	var item inboxItem
	if decodeErr := unmarshalJSONOutput(stdout, &item); decodeErr != nil {
		t.Fatalf("unmarshal read output: %v (output: %s)", decodeErr, stdout)
	}
	if item.ID != id || item.ParseError == "" || item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("partial read item = %#v", item)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md")); !os.IsNotExist(statErr) {
		t.Fatalf("partial read source remains in new: %v", statErr)
	}
	if _, statErr := os.Stat(wantSource); statErr != nil {
		t.Fatalf("partial read source missing from cur: %v", statErr)
	}
	if _, statErr := os.Stat(wantEnvelope); statErr != nil {
		t.Fatalf("partial read DLQ envelope missing: %v", statErr)
	}
	assertReceiptCount(t, root, "alice", id, receipt.StageDLQ, 0)
}

func TestRunReadOutputsCompletedDLQWhenSourceDirSyncFails(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "committed-read-dlq"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	injectedErr := installReadDLQSourceDirSyncFailure(t, id+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runRead([]string{"--root", root, "--me", "alice", "--id", id, "--json"})
	})

	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, injectedErr) || GetExitCode(err) != ExitError {
		t.Fatalf("read error = %T %v, want committed DLQ transition error", err, err)
	}
	if committed.FinalPath == "" || committed.Recipient != "alice" {
		t.Fatalf("committed transition metadata = %#v", committed)
	}
	var item inboxItem
	if decodeErr := unmarshalJSONOutput(stdout, &item); decodeErr != nil {
		t.Fatalf("unmarshal read output: %v (output: %s)", decodeErr, stdout)
	}
	if item.ID != id || item.ParseError == "" || !item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("completed read DLQ item = %#v", item)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md")); !os.IsNotExist(statErr) {
		t.Fatalf("completed read source remains in new: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")); !os.IsNotExist(statErr) {
		t.Fatalf("completed read source remains in cur: %v", statErr)
	}
	if _, statErr := os.Stat(committed.FinalPath); statErr != nil {
		t.Fatalf("completed read DLQ envelope missing: %v", statErr)
	}
	assertReceiptCount(t, root, "alice", id, receipt.StageDLQ, 1)
}

func TestRunDrainOutputsOrdinaryDLQPlacementFailureBeforeError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "ordinary-drain"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	injectedErr := installOrdinaryDLQPlacementFailure(t, id+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--json"})
	})
	assertOrdinaryDLQPlacementError(t, root, stdout, err, injectedErr, id)
}

func TestRunDrainCorruptMessageWithoutSenderDoesNotWarn(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "corrupt-without-sender"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("drain corrupt senderless message: %v", err)
	}
	if strings.Contains(stderr, "receipt sender normalization failed") {
		t.Fatalf("senderless corrupt message emitted unactionable warning: %s", stderr)
	}
	var result drainResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal senderless corrupt drain output: %v (output: %s)", decodeErr, stdout)
	}
	assertCompletedDLQResult(t, result.Drained, result.Count, id)
	assertCompletedDLQState(t, root, id)
	receipts, listErr := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: id,
		Stage: receipt.StageDLQ,
	})
	if listErr != nil || len(receipts) != 1 {
		t.Fatalf("senderless corrupt receipts = %#v, err=%v; want one", receipts, listErr)
	}
	gotReceipt := receipts[0]
	if gotReceipt.Stage != receipt.StageDLQ || gotReceipt.Sender != "" {
		t.Fatalf("senderless corrupt receipt = %#v, want stage dlq and empty sender", gotReceipt)
	}
	if !strings.Contains(gotReceipt.Detail, "missing frontmatter") ||
		!strings.Contains(gotReceipt.Detail, receiptSenderUnavailableDetail) {
		t.Fatalf("senderless corrupt receipt detail = %q, want parse error plus sender explanation", gotReceipt.Detail)
	}
}

func TestEmitReceiptStillWarnsForNonemptyInvalidSender(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	defer func() { _ = deliveryRoot.Close() }()

	_, stderr, err := captureEnvOutput(t, func() error {
		emitReceipt(deliveryRoot, "alice", &inboxItem{
			ID:   "invalid-receipt-sender",
			From: "not a valid sender",
		}, receipt.StageDLQ, "parse failed")
		return nil
	})
	if err != nil {
		t.Fatalf("emit invalid-sender receipt: %v", err)
	}
	if !strings.Contains(stderr, `receipt sender normalization failed for "not a valid sender"`) {
		t.Fatalf("nonempty invalid sender warning = %q", stderr)
	}
	receipts, listErr := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "invalid-receipt-sender",
		Stage: receipt.StageDLQ,
	})
	if listErr != nil || len(receipts) != 1 {
		t.Fatalf("invalid-sender receipts = %#v, err=%v; want one", receipts, listErr)
	}
	if receipts[0].Sender != "" || receipts[0].Detail != "parse failed" {
		t.Fatalf("invalid-sender receipt = %#v, want empty sender and unchanged detail", receipts[0])
	}
}

func TestRunMonitorInitialOutputsOrdinaryDLQPlacementFailureBeforeError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "ordinary-monitor-initial"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	injectedErr := installOrdinaryDLQPlacementFailure(t, id+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	assertOrdinaryDLQPlacementMonitorError(t, root, stdout, err, injectedErr, id, "existing")
}

func TestRunMonitorPostWatchOutputsOrdinaryDLQPlacementFailureBeforeError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "ordinary-monitor-watch"
	injectedErr := installOrdinaryDLQPlacementFailure(t, id+".md")

	idle := make(chan struct{})
	release := make(chan struct{})
	oldIdleHook := monitorPollingIdleForTest
	monitorPollingIdleForTest = func() {
		close(idle)
		<-release
	}
	t.Cleanup(func() { monitorPollingIdleForTest = oldIdleHook })

	deliveryErr := make(chan error, 1)
	go func() {
		<-idle
		deliveryErr <- deliverInvalidDLQTransitionFixtureError(root, "alice", id)
		close(release)
	}()

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	if deliveryErr := <-deliveryErr; deliveryErr != nil {
		t.Fatalf("deliver post-watch fixture: %v", deliveryErr)
	}
	assertOrdinaryDLQPlacementMonitorError(t, root, stdout, err, injectedErr, id, "new_message")
}

func TestRunDrainOutputsPartialDLQTransitionBeforeError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverInvalidDLQTransitionFixture(t, root, "alice", "partial-drain")
	injectedErr := installPartialDLQTransition(t, root, "alice", "partial-drain.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--json"})
	})
	assertPartialDLQTransitionOutput(t, root, stdout, err, injectedErr, "partial-drain")
}

func TestRunMonitorInitialOutputsPartialDLQTransitionBeforeError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverInvalidDLQTransitionFixture(t, root, "alice", "partial-monitor-initial")
	injectedErr := installPartialDLQTransition(t, root, "alice", "partial-monitor-initial.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	assertPartialDLQTransitionMonitorOutput(t, root, stdout, err, injectedErr, "partial-monitor-initial", "existing")
}

func TestRunMonitorPostWatchOutputsPartialDLQTransitionBeforeError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	injectedErr := installPartialDLQTransition(t, root, "alice", "partial-monitor-watch.md")

	idle := make(chan struct{})
	release := make(chan struct{})
	oldIdleHook := monitorPollingIdleForTest
	monitorPollingIdleForTest = func() {
		close(idle)
		<-release
	}
	t.Cleanup(func() { monitorPollingIdleForTest = oldIdleHook })

	deliveryErr := make(chan error, 1)
	go func() {
		<-idle
		deliveryErr <- deliverInvalidDLQTransitionFixtureError(root, "alice", "partial-monitor-watch")
		close(release)
	}()

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	if deliveryErr := <-deliveryErr; deliveryErr != nil {
		t.Fatalf("deliver post-watch fixture: %v", deliveryErr)
	}
	assertPartialDLQTransitionMonitorOutput(t, root, stdout, err, injectedErr, "partial-monitor-watch", "new_message")
}

func TestRunDrainOutputsCompletedDLQTransitionBeforeDurabilityError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverInvalidDLQTransitionFixture(t, root, "alice", "committed-dlq")
	injectedErr := installCommittedDLQTransition(t, "committed-dlq.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--json"})
	})

	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, injectedErr) {
		t.Fatalf("drain error = %T %v, want committed DLQ transition error", err, err)
	}
	var result drainResult
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("unmarshal drain output: %v (output: %s)", err, stdout)
	}
	if result.Count != 1 || len(result.Drained) != 1 || !result.Drained[0].MovedToDLQ {
		t.Fatalf("completed DLQ output = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), "committed-dlq.md")); !os.IsNotExist(err) {
		t.Fatalf("completed DLQ transition retained source: %v", err)
	}
	assertReceiptCount(t, root, "alice", "committed-dlq", receipt.StageDLQ, 1)
}

func TestRunDrainReconcilesCommittedClaimIntoDLQOutcome(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "claim-then-dlq"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	claimErr := installCommittedClaimAfterMove(t, id+".md")
	dlqErr := errors.New("injected reconciled DLQ durability failure")
	oldMove := moveClaimedInboxCurToDLQ
	moveClaimedInboxCurToDLQ = func(
		deliveryRoot *fsq.DeliveryRoot,
		agent, filename, originalID, failureReason, failureDetail string,
		gotClaimErr error,
	) (string, error) {
		dlqPath, err := oldMove(
			deliveryRoot,
			agent,
			filename,
			originalID,
			failureReason,
			failureDetail,
			gotClaimErr,
		)
		if err != nil {
			return dlqPath, err
		}
		return dlqPath, &fsq.CommittedDurabilityError{
			FinalPath: dlqPath,
			Recipient: agent,
			Err:       errors.Join(gotClaimErr, dlqErr),
		}
	}
	t.Cleanup(func() { moveClaimedInboxCurToDLQ = oldMove })

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--json"})
	})

	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("drain error = %T %v, want reconciled CommittedDurabilityError", err, err)
	}
	for _, cause := range []error{claimErr, dlqErr} {
		if !errors.Is(err, cause) {
			t.Fatalf("drain error = %v, want cause %v", err, cause)
		}
	}

	var result drainResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal reconciled drain output: %v (output: %s)", decodeErr, stdout)
	}
	assertCompletedDLQResult(t, result.Drained, result.Count, id)
	dlqPath := assertCompletedDLQState(t, root, id)
	if committed.FinalPath != dlqPath || committed.Recipient != "alice" {
		t.Fatalf("reconciled committed outcome = %#v, want DLQ path %q recipient alice", committed, dlqPath)
	}
}

func TestRunMonitorInitialRecoversCommittedClaimAfterDLQ(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "monitor-initial-claim-dlq"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)
	_ = installCommittedClaimAfterMove(t, id+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	if err != nil {
		t.Fatalf("monitor initial recovered claim error = %v, want nil", err)
	}

	var result monitorResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal recovered initial monitor output: %v (output: %s)", decodeErr, stdout)
	}
	if result.Event != "messages" || result.WatchEvent != "existing" {
		t.Fatalf("monitor initial event = (%q, %q), want (messages, existing)", result.Event, result.WatchEvent)
	}
	assertCompletedDLQResult(t, result.Drained, result.Count, id)
	assertCompletedDLQState(t, root, id)
}

func TestRunMonitorPostWatchRecoversCommittedClaimAfterDLQ(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "monitor-watch-claim-dlq"
	_ = installCommittedClaimAfterMove(t, id+".md")

	idle := make(chan struct{})
	release := make(chan struct{})
	oldIdleHook := monitorPollingIdleForTest
	monitorPollingIdleForTest = func() {
		close(idle)
		<-release
	}
	t.Cleanup(func() { monitorPollingIdleForTest = oldIdleHook })

	deliveryErr := make(chan error, 1)
	go func() {
		<-idle
		deliveryErr <- deliverInvalidDLQTransitionFixtureError(root, "alice", id)
		close(release)
	}()

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	if deliveryErr := <-deliveryErr; deliveryErr != nil {
		t.Fatalf("deliver post-watch recovered fixture: %v", deliveryErr)
	}
	if err != nil {
		t.Fatalf("monitor post-watch recovered claim error = %v, want nil", err)
	}

	var result monitorResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal recovered post-watch monitor output: %v (output: %s)", decodeErr, stdout)
	}
	if result.Event != "messages" || result.WatchEvent != "new_message" {
		t.Fatalf("monitor post-watch event = (%q, %q), want (messages, new_message)", result.Event, result.WatchEvent)
	}
	assertCompletedDLQResult(t, result.Drained, result.Count, id)
	assertCompletedDLQState(t, root, id)
}

func assertCompletedDLQResult(t *testing.T, items []inboxItem, count int, id string) {
	t.Helper()
	if count != 1 || len(items) != 1 {
		t.Fatalf("completed DLQ output count = %d items = %#v, want 1", count, items)
	}
	item := items[0]
	if item.ID != id || item.ParseError == "" || !item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("completed DLQ item = %#v", item)
	}
}

func assertCompletedDLQState(t *testing.T, root, id string) string {
	t.Helper()
	for _, path := range []string{
		filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md"),
		filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("completed DLQ source remains at %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(fsq.AgentDLQNew(root, "alice"))
	if err != nil {
		t.Fatalf("read completed DLQ directory: %v", err)
	}
	if len(entries) != 1 || entries[0].IsDir() {
		t.Fatalf("completed DLQ entries = %#v, want one envelope", entries)
	}
	dlqPath := filepath.Join(fsq.AgentDLQNew(root, "alice"), entries[0].Name())
	env, original, err := fsq.ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read completed DLQ envelope: %v", err)
	}
	if env.OriginalID != id || string(original) != "missing frontmatter" {
		t.Fatalf("completed DLQ envelope = %#v body=%q, want original %q", env, original, id)
	}
	assertReceiptCount(t, root, "alice", id, receipt.StageDLQ, 1)
	return dlqPath
}

func installReadDLQSourceRemovalFailure(t *testing.T, root, agent, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected partial read DLQ transition")
	oldMove := moveReadMessageToDLQ
	moveReadMessageToDLQ = func(deliveryRoot *fsq.DeliveryRoot, gotAgent, filename, originalID, failureReason, failureDetail string) (string, error) {
		if gotAgent != agent || filename != targetFilename {
			return oldMove(deliveryRoot, gotAgent, filename, originalID, failureReason, failureDetail)
		}
		if err := fsq.MoveNewToCur(deliveryRoot, gotAgent, filename); err != nil {
			return "", err
		}
		dlqPath := filepath.Join(fsq.AgentDLQNew(root, agent), "partial-"+filename)
		if err := os.WriteFile(dlqPath, []byte("visible partial envelope"), 0o600); err != nil {
			return "", err
		}
		sourcePath := filepath.Join(fsq.AgentInboxCur(root, agent), filename)
		return dlqPath, &fsq.DLQTransitionError{
			EnvelopePath:   dlqPath,
			SourcePath:     sourcePath,
			SourceRetained: true,
			Err:            injectedErr,
		}
	}
	t.Cleanup(func() { moveReadMessageToDLQ = oldMove })
	return injectedErr
}

func installReadCommittedClaimOnDLQPlacementFailure(t *testing.T, targetFilename string) error {
	t.Helper()
	claimErr := errors.New("injected committed read claim sync failure")
	oldMove := moveReadMessageToDLQ
	moveReadMessageToDLQ = func(root *fsq.DeliveryRoot, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
		dlqPath, err := oldMove(root, agent, filename, originalID, failureReason, failureDetail)
		if filename != targetFilename || err == nil {
			return dlqPath, err
		}
		finalPath := root.DisplayPath(filepath.Join("agents", agent, "inbox", "cur", filename))
		return dlqPath, errors.Join(
			&fsq.CommittedDurabilityError{
				FinalPath: finalPath,
				Recipient: agent,
				Err:       claimErr,
			},
			err,
		)
	}
	t.Cleanup(func() { moveReadMessageToDLQ = oldMove })
	return claimErr
}

func installReadDLQSourceDirSyncFailure(t *testing.T, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected completed read DLQ durability failure")
	oldMove := moveReadMessageToDLQ
	moveReadMessageToDLQ = func(root *fsq.DeliveryRoot, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
		dlqPath, err := oldMove(root, agent, filename, originalID, failureReason, failureDetail)
		if err != nil || filename != targetFilename {
			return dlqPath, err
		}
		return dlqPath, &fsq.CommittedDurabilityError{
			FinalPath: dlqPath,
			Recipient: agent,
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { moveReadMessageToDLQ = oldMove })
	return injectedErr
}

func installPartialDLQTransition(t *testing.T, root, agent, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected partial DLQ transition")
	oldMove := moveInboxCurToDLQ
	moveInboxCurToDLQ = func(deliveryRoot *fsq.DeliveryRoot, gotAgent, filename, originalID, failureReason, failureDetail string) (string, error) {
		if gotAgent != agent || filename != targetFilename {
			return oldMove(deliveryRoot, gotAgent, filename, originalID, failureReason, failureDetail)
		}
		dlqPath := filepath.Join(fsq.AgentDLQNew(root, agent), "partial-"+filename)
		if err := os.WriteFile(dlqPath, []byte("visible partial envelope"), 0o600); err != nil {
			return "", err
		}
		sourcePath := filepath.Join(fsq.AgentInboxCur(root, agent), filename)
		return dlqPath, &fsq.DLQTransitionError{
			EnvelopePath:   dlqPath,
			SourcePath:     sourcePath,
			SourceRetained: true,
			Err:            injectedErr,
		}
	}
	t.Cleanup(func() { moveInboxCurToDLQ = oldMove })
	return injectedErr
}

func installCommittedDLQTransition(t *testing.T, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected completed DLQ durability failure")
	oldMove := moveInboxCurToDLQ
	moveInboxCurToDLQ = func(root *fsq.DeliveryRoot, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
		dlqPath, err := oldMove(root, agent, filename, originalID, failureReason, failureDetail)
		if err != nil || filename != targetFilename {
			return dlqPath, err
		}
		return dlqPath, &fsq.CommittedDurabilityError{
			FinalPath: dlqPath,
			Recipient: agent,
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { moveInboxCurToDLQ = oldMove })
	return injectedErr
}

func installOrdinaryDLQPlacementFailure(t *testing.T, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected pre-envelope DLQ placement failure")
	oldMove := moveInboxCurToDLQ
	moveInboxCurToDLQ = func(root *fsq.DeliveryRoot, agent, filename, originalID, failureReason, failureDetail string) (string, error) {
		if filename != targetFilename {
			return oldMove(root, agent, filename, originalID, failureReason, failureDetail)
		}
		return "", injectedErr
	}
	t.Cleanup(func() { moveInboxCurToDLQ = oldMove })
	return injectedErr
}

func blockDLQTmpWithRegularFile(t *testing.T, root, agent string) {
	t.Helper()
	tmp := fsq.AgentDLQTmp(root, agent)
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatalf("remove DLQ tmp: %v", err)
	}
	if err := os.WriteFile(tmp, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block DLQ tmp: %v", err)
	}
}

func assertOrdinaryDLQPlacementMonitorError(t *testing.T, root, stdout string, err, injectedErr error, id, wantWatchEvent string) {
	t.Helper()
	var result monitorResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal monitor output: %v (output: %s)", decodeErr, stdout)
	}
	if result.Event != "messages" || result.WatchEvent != wantWatchEvent {
		t.Fatalf("monitor event = (%q, %q), want (messages, %q)", result.Event, result.WatchEvent, wantWatchEvent)
	}
	assertOrdinaryDLQPlacementResult(t, root, result.Drained, result.Count, err, injectedErr, id)
}

func assertOrdinaryDLQPlacementError(t *testing.T, root, stdout string, err, injectedErr error, id string) {
	t.Helper()
	var result drainResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal drain output: %v (output: %s)", decodeErr, stdout)
	}
	assertOrdinaryDLQPlacementResult(t, root, result.Drained, result.Count, err, injectedErr, id)
}

func assertOrdinaryDLQPlacementResult(t *testing.T, root string, items []inboxItem, count int, err, injectedErr error, id string) {
	t.Helper()
	if !errors.Is(err, injectedErr) || GetExitCode(err) != ExitError {
		t.Fatalf("command error = %T %v (exit %d), want ordinary injected error", err, err, GetExitCode(err))
	}
	var partial *fsq.DLQTransitionError
	var committed *fsq.CommittedDurabilityError
	if errors.As(err, &partial) || errors.As(err, &committed) {
		t.Fatalf("command error = %T %v, want ordinary pre-envelope failure", err, err)
	}
	if count != 1 || len(items) != 1 {
		t.Fatalf("ordinary DLQ failure output count = %d items = %#v", count, items)
	}
	item := items[0]
	if item.ID != id || item.ParseError == "" || item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("ordinary DLQ failure item = %#v", item)
	}
	assertOrdinaryDLQFailureState(t, root, id)
}

func assertOrdinaryDLQFailureState(t *testing.T, root, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md")); !os.IsNotExist(err) {
		t.Fatalf("ordinary DLQ failure source remains in new: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")); err != nil {
		t.Fatalf("ordinary DLQ failure source missing from cur: %v", err)
	}
	dlqEntries, err := os.ReadDir(fsq.AgentDLQNew(root, "alice"))
	if err != nil {
		t.Fatalf("read DLQ new: %v", err)
	}
	if len(dlqEntries) != 0 {
		t.Fatalf("ordinary DLQ failure envelopes = %d, want 0", len(dlqEntries))
	}
	assertReceiptCount(t, root, "alice", id, receipt.StageDLQ, 0)
}

func assertPartialDLQTransitionMonitorOutput(t *testing.T, root, stdout string, err, injectedErr error, id, wantWatchEvent string) {
	t.Helper()
	var result monitorResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal monitor output: %v (output: %s)", decodeErr, stdout)
	}
	if result.Event != "messages" || result.WatchEvent != wantWatchEvent {
		t.Fatalf("monitor event = (%q, %q), want (messages, %q)", result.Event, result.WatchEvent, wantWatchEvent)
	}
	assertPartialDLQTransitionResult(t, root, result.Drained, result.Count, err, injectedErr, id)
}

func assertPartialDLQTransitionOutput(t *testing.T, root, stdout string, err, injectedErr error, id string) {
	t.Helper()
	var result drainResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal drain output: %v (output: %s)", decodeErr, stdout)
	}
	assertPartialDLQTransitionResult(t, root, result.Drained, result.Count, err, injectedErr, id)
}

func assertPartialDLQTransitionResult(t *testing.T, root string, items []inboxItem, count int, err, injectedErr error, id string) {
	t.Helper()
	var transition *fsq.DLQTransitionError
	if !errors.As(err, &transition) || !errors.Is(err, injectedErr) {
		t.Fatalf("command error = %T %v, want partial DLQ transition error", err, err)
	}
	if count != 1 || len(items) != 1 {
		t.Fatalf("partial DLQ output count = %d items = %#v", count, items)
	}
	item := items[0]
	if item.ID != id || item.ParseError == "" || item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("partial DLQ item = %#v", item)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")); err != nil {
		t.Fatalf("partial DLQ source missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentDLQNew(root, "alice"), "partial-"+id+".md")); err != nil {
		t.Fatalf("partial DLQ envelope missing: %v", err)
	}
	assertReceiptCount(t, root, "alice", id, receipt.StageDLQ, 0)
}

func deliverInvalidDLQTransitionFixture(t *testing.T, root, agent, id string) {
	t.Helper()
	if err := deliverInvalidDLQTransitionFixtureError(root, agent, id); err != nil {
		t.Fatalf("deliver invalid fixture: %v", err)
	}
}

func deliverInvalidDLQTransitionFixtureError(root, agent, id string) error {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	_, err = fsq.DeliverToInbox(deliveryRoot, agent, id+".md", []byte("missing frontmatter"))
	return err
}

func assertReceiptCount(t *testing.T, root, agent, id, stage string, want int) {
	t.Helper()
	receipts, err := receipt.List(root, agent, receipt.ListFilter{MsgID: id, Stage: stage})
	if err != nil {
		t.Fatalf("list %s receipts: %v", stage, err)
	}
	if len(receipts) != want {
		t.Fatalf("%s receipts for %s = %d, want %d", stage, id, len(receipts), want)
	}
}
