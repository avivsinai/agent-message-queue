package fsq

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveToDLQ(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a corrupt message in inbox/new
	inboxNew := AgentInboxNew(root, "alice")
	filename := "corrupt_123.md"
	content := []byte("not valid frontmatter at all")
	if err := os.WriteFile(filepath.Join(inboxNew, filename), content, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	// Move to DLQ
	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "corrupt_123", "parse_error", "missing frontmatter")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	// Verify original removed from inbox/new
	if _, err := os.Stat(filepath.Join(inboxNew, filename)); !os.IsNotExist(err) {
		t.Errorf("original should be removed from inbox/new")
	}

	// Verify DLQ message exists
	if _, err := os.Stat(dlqPath); err != nil {
		t.Errorf("DLQ message should exist: %v", err)
	}

	// Verify DLQ envelope content
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope: %v", err)
	}

	if env.Schema != DLQSchemaVersion {
		t.Errorf("expected schema %s, got %s", DLQSchemaVersion, env.Schema)
	}
	if env.OriginalID != "corrupt_123" {
		t.Errorf("expected original_id corrupt_123, got %s", env.OriginalID)
	}
	if env.OriginalFile != filename {
		t.Errorf("expected original_file %s, got %s", filename, env.OriginalFile)
	}
	if env.FailureReason != "parse_error" {
		t.Errorf("expected failure_reason parse_error, got %s", env.FailureReason)
	}
	if env.RetryCount != 0 {
		t.Errorf("expected retry_count 0, got %d", env.RetryCount)
	}
	if string(body) != string(content) {
		t.Errorf("body mismatch: expected %q, got %q", content, body)
	}
}

func TestMoveToDLQFailsWhenRandomReadFails(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	filename := "need_dlq.md"
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, "alice"), filename), []byte("payload"), 0o600); err != nil {
		t.Fatalf("write inbox message: %v", err)
	}

	injected := errors.New("injected rand.Read failure")
	orig := readRandom
	readRandom = func([]byte) (int, error) { return 0, injected }
	t.Cleanup(func() { readRandom = orig })

	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "need_dlq", "parse_error", "bad")
	if err == nil {
		t.Fatal("MoveToDLQ error = nil, want RNG failure")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("MoveToDLQ error = %v, want injected rand.Read failure", err)
	}
	if dlqPath != "" {
		t.Fatalf("dlqPath = %q, want empty", dlqPath)
	}

	for _, dir := range []string{AgentDLQNew(root, "alice"), AgentDLQTmp(root, "alice"), AgentDLQCur(root, "alice")} {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatalf("ReadDir %s: %v", dir, readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("%s has files after RNG failure: %#v", dir, entries)
		}
	}
}

func TestMoveToDLQClaimsBeforeDLQDelivery(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	filename := "claim_before_dlq.md"
	content := []byte("not valid frontmatter")
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, "alice"), filename), content, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	dlqTmp := AgentDLQTmp(root, "alice")
	if err := os.RemoveAll(dlqTmp); err != nil {
		t.Fatalf("remove dlq tmp: %v", err)
	}
	if err := os.WriteFile(dlqTmp, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block dlq tmp: %v", err)
	}

	_, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "claim_before_dlq", "parse_error", "missing frontmatter")
	if err == nil {
		t.Fatal("expected MoveToDLQ to fail when DLQ delivery cannot create tmp dir")
	}

	if _, err := os.Stat(filepath.Join(AgentInboxNew(root, "alice"), filename)); !os.IsNotExist(err) {
		t.Fatalf("source should be claimed out of inbox/new before DLQ delivery, stat err: %v", err)
	}
	claimedContent, err := os.ReadFile(filepath.Join(AgentInboxCur(root, "alice"), filename))
	if err != nil {
		t.Fatalf("claimed source should remain in inbox/cur: %v", err)
	}
	if string(claimedContent) != string(content) {
		t.Fatalf("claimed content mismatch: got %q", claimedContent)
	}
}

func TestMoveToDLQRecoversOneShotCommittedClaimSyncFailure(t *testing.T) {
	const (
		agent    = "alice"
		filename = "recover_claim.md"
	)
	content := []byte("corrupt payload")

	for _, failureDir := range []string{
		filepath.Join("agents", agent, "inbox", "new"),
		filepath.Join("agents", agent, "inbox", "cur"),
	} {
		t.Run(filepath.Base(failureDir), func(t *testing.T) {
			root := t.TempDir()
			if err := EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}
			if err := os.WriteFile(filepath.Join(AgentInboxNew(root, agent), filename), content, 0o600); err != nil {
				t.Fatalf("write source: %v", err)
			}

			deliveryRoot := openDeliveryRootForTest(t, root)
			injectedErr := errors.New("injected one-shot claim sync failure")
			faults := 0
			deliveryRoot.syncDirForTest = func(dir string) error {
				if dir == failureDir && faults == 0 {
					faults++
					return injectedErr
				}
				return deliveryRoot.syncDirPlatform(dir)
			}

			dlqPath, err := MoveToDLQ(
				deliveryRoot,
				agent,
				filename,
				"recover_claim",
				"parse_error",
				"bad payload",
			)
			if err != nil {
				t.Fatalf("MoveToDLQ after transient committed claim: %v", err)
			}
			if faults != 1 {
				t.Fatalf("injected claim sync faults = %d, want 1", faults)
			}
			if dlqPath == "" {
				t.Fatal("recovered DLQ transition returned an empty path")
			}

			env, body, err := ReadDLQEnvelopePath(dlqPath)
			if err != nil {
				t.Fatalf("read recovered DLQ envelope: %v", err)
			}
			if env.OriginalFile != filename || string(body) != string(content) {
				t.Fatalf("recovered envelope = (%q, %q), want (%q, %q)", env.OriginalFile, body, filename, content)
			}
			for _, sourcePath := range []string{
				filepath.Join(AgentInboxNew(root, agent), filename),
				filepath.Join(AgentInboxCur(root, agent), filename),
			} {
				if _, statErr := os.Stat(sourcePath); !os.IsNotExist(statErr) {
					t.Fatalf("recovered transition retained source %s: %v", sourcePath, statErr)
				}
			}
		})
	}
}

func TestMoveToDLQPersistentCommittedClaimSyncFailureReturnsTransition(t *testing.T) {
	const (
		agent    = "alice"
		filename = "persistent_claim.md"
	)
	content := []byte("corrupt payload")

	for _, failureDir := range []string{
		filepath.Join("agents", agent, "inbox", "new"),
		filepath.Join("agents", agent, "inbox", "cur"),
	} {
		t.Run(filepath.Base(failureDir), func(t *testing.T) {
			root := t.TempDir()
			if err := EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}
			if err := os.WriteFile(filepath.Join(AgentInboxNew(root, agent), filename), content, 0o600); err != nil {
				t.Fatalf("write source: %v", err)
			}

			deliveryRoot := openDeliveryRootForTest(t, root)
			injectedErr := errors.New("injected persistent claim sync failure")
			deliveryRoot.syncDirForTest = func(dir string) error {
				if dir == failureDir {
					return injectedErr
				}
				return deliveryRoot.syncDirPlatform(dir)
			}

			dlqPath, err := MoveToDLQ(
				deliveryRoot,
				agent,
				filename,
				"persistent_claim",
				"parse_error",
				"bad payload",
			)
			if err == nil {
				t.Fatal("MoveToDLQ persistent fault error = nil, want typed partial transition")
			}
			if dlqPath == "" {
				t.Fatal("persistent claim fault discarded the visible DLQ envelope path")
			}
			var committed *CommittedDurabilityError
			if !errors.As(err, &committed) {
				t.Fatalf("MoveToDLQ error = %T %v, want original committed claim", err, err)
			}
			if !errors.Is(err, injectedErr) {
				t.Fatalf("MoveToDLQ error = %v, want injected sync failure", err)
			}
			if committed.FinalPath != dlqPath || committed.Recipient != agent {
				t.Fatalf("committed transition = (%q,%q), want (%q,%q)", committed.FinalPath, committed.Recipient, dlqPath, agent)
			}
			if _, statErr := os.Stat(dlqPath); statErr != nil {
				t.Fatalf("visible DLQ envelope missing: %v", statErr)
			}
			for _, sourcePath := range []string{
				filepath.Join(AgentInboxNew(root, agent), filename),
				filepath.Join(AgentInboxCur(root, agent), filename),
			} {
				if _, statErr := os.Stat(sourcePath); !os.IsNotExist(statErr) {
					t.Fatalf("completed transition retained source %s: %v", sourcePath, statErr)
				}
			}
		})
	}
}

func TestMoveToDLQCommittedClaimAndRetainedDLQPartialStaysPartial(t *testing.T) {
	const (
		agent    = "alice"
		filename = "claim_and_dlq_partial.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	sourcePath := filepath.Join(AgentInboxNew(root, agent), filename)
	content := []byte("corrupt payload")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	deliveryRoot := openDeliveryRootForTest(t, root)
	claimErr := errors.New("injected committed claim sync failure")
	dlqErr := errors.New("injected DLQ envelope sync failure")
	claimFaults := 0
	deliveryRoot.syncDirForTest = func(dir string) error {
		switch dir {
		case filepath.Join("agents", agent, "inbox", "cur"):
			if claimFaults == 0 {
				claimFaults++
				return claimErr
			}
		case filepath.Join("agents", agent, "dlq", "new"):
			return dlqErr
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	dlqPath, err := MoveToDLQ(
		deliveryRoot,
		agent,
		filename,
		"claim_and_dlq_partial",
		"parse_error",
		"bad payload",
	)
	if err == nil {
		t.Fatal("MoveToDLQ combined fault error = nil, want retained partial transition")
	}
	var partial *DLQTransitionError
	if !errors.As(err, &partial) || !partial.SourceRetained {
		t.Fatalf("MoveToDLQ combined fault = %T %v, want retained DLQTransitionError", err, err)
	}
	if partial.EnvelopePath != dlqPath {
		t.Fatalf("partial envelope path = %q, want %q", partial.EnvelopePath, dlqPath)
	}
	if !errors.Is(err, claimErr) || !errors.Is(err, dlqErr) {
		t.Fatalf("combined error = %v, want both injected causes", err)
	}
	if _, statErr := os.Stat(dlqPath); statErr != nil {
		t.Fatalf("visible DLQ envelope missing: %v", statErr)
	}
	curPath := filepath.Join(AgentInboxCur(root, agent), filename)
	if got, readErr := os.ReadFile(curPath); readErr != nil || string(got) != string(content) {
		t.Fatalf("retained source = %q, err=%v; want %q", got, readErr, content)
	}
}

func TestMoveClaimedCurToDLQReconcilesCommittedClaim(t *testing.T) {
	const (
		agent    = "alice"
		filename = "claimed-corrupt.md"
	)

	for _, test := range []struct {
		name                 string
		failTransitionCur    bool
		failReconciliation   bool
		wantReconcileCauses  bool
		wantReconciledToPass bool
	}{
		{
			name:                 "transient claim uncertainty recovers",
			wantReconciledToPass: true,
		},
		{
			name:                "persistent reconciliation failure names DLQ",
			failReconciliation:  true,
			wantReconcileCauses: true,
		},
		{
			name:                 "committed DLQ transition recovers after successful reconciliation",
			failTransitionCur:    true,
			wantReconciledToPass: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			if err := EnsureAgentDirs(base, agent); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}
			content := []byte("claimed corrupt payload")
			newPath := filepath.Join(AgentInboxNew(base, agent), filename)
			if err := os.WriteFile(newPath, content, 0o600); err != nil {
				t.Fatalf("write claimed fixture: %v", err)
			}

			root := openDeliveryRootForTest(t, base)
			newDir := filepath.Join("agents", agent, "inbox", "new")
			curDir := filepath.Join("agents", agent, "inbox", "cur")
			claimCause := errors.New("injected committed claim sync failure")
			claimFaults := 0
			root.syncDirForTest = func(dir string) error {
				if dir == curDir && claimFaults == 0 {
					claimFaults++
					return claimCause
				}
				return root.syncDirPlatform(dir)
			}
			claimErr := MoveNewToCur(root, agent, filename)
			var claimCommitted *CommittedDurabilityError
			if !errors.As(claimErr, &claimCommitted) || !errors.Is(claimErr, claimCause) {
				t.Fatalf("claim error = %T %v, want committed claim", claimErr, claimErr)
			}

			transitionCause := errors.New("injected committed DLQ transition sync failure")
			newSyncCause := errors.New("injected inbox/new reconciliation failure")
			curSyncCause := errors.New("injected inbox/cur reconciliation failure")
			syncCalls := make(map[string]int)
			root.syncDirForTest = func(dir string) error {
				syncCalls[dir]++
				switch {
				case dir == curDir && syncCalls[dir] == 1 && test.failTransitionCur:
					return transitionCause
				case dir == newDir && test.failReconciliation:
					return newSyncCause
				case dir == curDir && syncCalls[dir] == 2 && test.failReconciliation:
					return curSyncCause
				default:
					return root.syncDirPlatform(dir)
				}
			}

			dlqPath, err := MoveClaimedCurToDLQ(
				root,
				agent,
				filename,
				"claimed-corrupt",
				"parse_error",
				"bad payload",
				claimErr,
			)
			if test.wantReconciledToPass {
				if err != nil {
					t.Fatalf("recovered claimed DLQ transition: %v", err)
				}
			} else {
				var committed *CommittedDurabilityError
				if !errors.As(err, &committed) {
					t.Fatalf("claimed DLQ transition error = %T %v, want CommittedDurabilityError", err, err)
				}
				if committed.FinalPath != dlqPath || committed.Recipient != agent {
					t.Fatalf("committed DLQ outcome = %#v, want path %q recipient %q", committed, dlqPath, agent)
				}
				if !errors.Is(err, claimCause) {
					t.Fatalf("claimed DLQ transition error = %v, want original claim cause", err)
				}
			}
			if test.wantReconcileCauses {
				for _, cause := range []error{newSyncCause, curSyncCause} {
					if !errors.Is(err, cause) {
						t.Fatalf("claimed DLQ transition error = %v, want reconciliation cause %v", err, cause)
					}
				}
			}
			if syncCalls[newDir] != 1 || syncCalls[curDir] != 2 {
				t.Fatalf(
					"inbox sync calls = new:%d cur:%d, want reconciliation new:1 and transition+reconciliation cur:2",
					syncCalls[newDir],
					syncCalls[curDir],
				)
			}
			if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
				t.Fatalf("claimed DLQ source remains in inbox/new: %v", statErr)
			}
			curPath := filepath.Join(AgentInboxCur(base, agent), filename)
			if _, statErr := os.Stat(curPath); !os.IsNotExist(statErr) {
				t.Fatalf("claimed DLQ source remains in inbox/cur: %v", statErr)
			}
			env, original, readErr := ReadDLQEnvelopePath(dlqPath)
			if readErr != nil || env.OriginalID != "claimed-corrupt" || !bytes.Equal(original, content) {
				t.Fatalf("claimed DLQ envelope = %#v body=%q err=%v", env, original, readErr)
			}
		})
	}
}

func TestMoveClaimedCurToDLQRejectsMismatchedClaimProvenanceBeforeMutation(t *testing.T) {
	const (
		agent    = "alice"
		filename = "claimed-corrupt.md"
	)

	for _, test := range []struct {
		name      string
		recipient string
		finalPath func(root, foreignRoot string) string
	}{
		{
			name:      "wrong recipient",
			recipient: "bob",
			finalPath: func(root, _ string) string {
				return filepath.Join(AgentInboxCur(root, agent), filename)
			},
		},
		{
			name:      "wrong cur path",
			recipient: agent,
			finalPath: func(root, _ string) string {
				return filepath.Join(AgentInboxCur(root, agent), "another-message.md")
			},
		},
		{
			name:      "foreign delivery root",
			recipient: agent,
			finalPath: func(_, foreignRoot string) string {
				return filepath.Join(AgentInboxCur(foreignRoot, agent), filename)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			foreignBase := t.TempDir()
			for _, root := range []string{base, foreignBase} {
				if err := EnsureAgentDirs(root, agent); err != nil {
					t.Fatalf("EnsureAgentDirs(%s): %v", root, err)
				}
			}

			content := []byte("claimed corrupt payload")
			curPath := filepath.Join(AgentInboxCur(base, agent), filename)
			if err := os.WriteFile(curPath, content, 0o600); err != nil {
				t.Fatalf("write claimed fixture: %v", err)
			}
			claimCause := errors.New("injected committed claim sync failure")
			claimErr := &CommittedDurabilityError{
				FinalPath: test.finalPath(base, foreignBase),
				Recipient: test.recipient,
				Err:       claimCause,
			}

			root := openDeliveryRootForTest(t, base)
			dlqPath, err := MoveClaimedCurToDLQ(
				root,
				agent,
				filename,
				"claimed-corrupt",
				"parse_error",
				"bad payload",
				claimErr,
			)
			if err == nil {
				t.Fatal("mismatched committed claim error = nil, want rejection")
			}
			if dlqPath != "" {
				t.Fatalf("mismatched committed claim DLQ path = %q, want empty", dlqPath)
			}
			if !errors.Is(err, claimCause) {
				t.Fatalf("mismatched committed claim error = %v, want original claim cause", err)
			}
			if got, readErr := os.ReadFile(curPath); readErr != nil || !bytes.Equal(got, content) {
				t.Fatalf("retained cur source = %q, err=%v; want %q", got, readErr, content)
			}
			for _, dir := range []string{
				AgentDLQTmp(base, agent),
				AgentDLQNew(base, agent),
			} {
				entries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatalf("read DLQ directory %s: %v", dir, readErr)
				}
				if len(entries) != 0 {
					t.Fatalf("DLQ directory %s entries = %#v, want none", dir, entries)
				}
			}
		})
	}
}

func TestMoveCurToDLQ(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	filename := "claimed_corrupt.md"
	content := []byte("not valid frontmatter after claim")
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, "alice"), filename), content, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if err := MoveNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}

	dlqPath, err := MoveCurToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "claimed_corrupt", "parse_error", "missing frontmatter")
	if err != nil {
		t.Fatalf("MoveCurToDLQ: %v", err)
	}

	if _, err := os.Stat(filepath.Join(AgentInboxCur(root, "alice"), filename)); !os.IsNotExist(err) {
		t.Fatalf("claimed original should be removed from inbox/cur")
	}

	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope: %v", err)
	}
	if env.SourceDir != BoxCur {
		t.Fatalf("expected source_dir %q, got %q", BoxCur, env.SourceDir)
	}
	if env.OriginalFile != filename {
		t.Fatalf("expected original_file %q, got %q", filename, env.OriginalFile)
	}
	if string(body) != string(content) {
		t.Fatalf("body mismatch: expected %q, got %q", content, body)
	}
}

func TestMoveCurToDLQPostRenameSyncFailureReportsRetainedTransition(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	filename := "indeterminate_dlq.md"
	sourcePath := filepath.Join(AgentInboxCur(root, "alice"), filename)
	if err := os.WriteFile(sourcePath, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	deliveryRoot := openDeliveryRootForTest(t, root)
	dlqNewDir := filepath.Join("agents", "alice", "dlq", "new")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == dlqNewDir {
			return errors.New("injected post-rename sync failure")
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	dlqPath, err := MoveCurToDLQ(deliveryRoot, "alice", filename, "indeterminate_dlq", "parse_error", "bad data")
	if err == nil {
		t.Fatal("MoveCurToDLQ error = nil, want partial transition")
	}
	if dlqPath == "" {
		t.Fatal("MoveCurToDLQ discarded the committed envelope path")
	}
	var transition *DLQTransitionError
	if !errors.As(err, &transition) {
		t.Fatalf("error = %T %v, want typed DLQ transition", err, err)
	}
	if transition.EnvelopePath != dlqPath || transition.SourcePath != sourcePath || !transition.SourceRetained {
		t.Fatalf("transition = (%q,%q,%v), want (%q,%q,true)", transition.EnvelopePath, transition.SourcePath, transition.SourceRetained, dlqPath, sourcePath)
	}
	if _, statErr := os.Stat(dlqPath); statErr != nil {
		t.Fatalf("committed DLQ envelope missing: %v", statErr)
	}
	if _, statErr := os.Stat(sourcePath); statErr != nil {
		t.Fatalf("source was not retained: %v", statErr)
	}
}

func TestMoveCurToDLQSourceRemovalFailureReportsRetainedTransition(t *testing.T) {
	const (
		agent    = "alice"
		filename = "remove_failure.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	sourcePath := filepath.Join(AgentInboxCur(root, agent), filename)
	content := []byte("corrupt")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	injectedErr := errors.New("injected source removal failure")
	oldRemove := removeDLQSource
	removeDLQSource = func(root *DeliveryRoot, path string) error {
		if path == filepath.Join("agents", agent, "inbox", "cur", filename) {
			return injectedErr
		}
		return oldRemove(root, path)
	}
	t.Cleanup(func() { removeDLQSource = oldRemove })

	dlqPath, err := MoveCurToDLQ(
		openDeliveryRootForTest(t, root),
		agent,
		filename,
		"remove_failure",
		"parse_error",
		"bad data",
	)
	if err == nil {
		t.Fatal("MoveCurToDLQ removal failure error = nil, want typed partial transition")
	}
	var transition *DLQTransitionError
	if !errors.As(err, &transition) {
		t.Fatalf("MoveCurToDLQ error = %T %v, want DLQTransitionError", err, err)
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("MoveCurToDLQ error = %v, want injected removal failure", err)
	}
	if transition.EnvelopePath != dlqPath ||
		transition.SourcePath != sourcePath ||
		!transition.SourceRetained {
		t.Fatalf(
			"transition = (%q,%q,%v), want (%q,%q,true)",
			transition.EnvelopePath,
			transition.SourcePath,
			transition.SourceRetained,
			dlqPath,
			sourcePath,
		)
	}
	if _, statErr := os.Stat(dlqPath); statErr != nil {
		t.Fatalf("visible DLQ envelope missing: %v", statErr)
	}
	if got, readErr := os.ReadFile(sourcePath); readErr != nil || string(got) != string(content) {
		t.Fatalf("retained source = %q, err=%v; want %q", got, readErr, content)
	}
}

func TestMoveCurToDLQSourceRemovalSyncFailureReportsCompletedTransition(t *testing.T) {
	const (
		agent    = "alice"
		filename = "remove_sync_failure.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	sourcePath := filepath.Join(AgentInboxCur(root, agent), filename)
	if err := os.WriteFile(sourcePath, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	deliveryRoot := openDeliveryRootForTest(t, root)
	injectedErr := errors.New("injected removed-source directory sync failure")
	sourceDir := filepath.Join("agents", agent, "inbox", "cur")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == sourceDir {
			return injectedErr
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	dlqPath, err := MoveCurToDLQ(
		deliveryRoot,
		agent,
		filename,
		"remove_sync_failure",
		"parse_error",
		"bad data",
	)
	if err == nil {
		t.Fatal("MoveCurToDLQ source sync failure error = nil, want typed indeterminate transition")
	}
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("MoveCurToDLQ error = %T %v, want CommittedDurabilityError", err, err)
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("MoveCurToDLQ error = %v, want injected sync failure", err)
	}
	if committed.FinalPath != dlqPath || committed.Recipient != agent {
		t.Fatalf("committed transition = (%q,%q), want (%q,%q)", committed.FinalPath, committed.Recipient, dlqPath, agent)
	}
	if _, statErr := os.Stat(dlqPath); statErr != nil {
		t.Fatalf("visible DLQ envelope missing: %v", statErr)
	}
	if _, statErr := os.Stat(sourcePath); !os.IsNotExist(statErr) {
		t.Fatalf("source removal is not visible: %v", statErr)
	}
}

func TestRetryFromDLQ(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message in inbox/new first
	inboxNew := AgentInboxNew(root, "alice")
	filename := "test_msg.md"
	content := []byte("---\n{\"schema\":1,\"id\":\"test_msg\"}\n---\nHello")
	if err := os.WriteFile(filepath.Join(inboxNew, filename), content, 0o600); err != nil {
		t.Fatalf("write test msg: %v", err)
	}

	// Move to DLQ
	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "test_msg", "test_failure", "test detail")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	dlqFilename := filepath.Base(dlqPath)

	// Retry from DLQ
	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, false); err != nil {
		t.Fatalf("RetryFromDLQ: %v", err)
	}

	// Verify message back in inbox/new
	inboxPath := filepath.Join(inboxNew, filename)
	restoredContent, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("read restored message: %v", err)
	}
	if string(restoredContent) != string(content) {
		t.Errorf("restored content mismatch")
	}

	// Verify DLQ envelope moved to cur with incremented retry count
	dlqCur := AgentDLQCur(root, "alice")
	curPath := filepath.Join(dlqCur, dlqFilename)
	env, _, err := ReadDLQEnvelopePath(curPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope from cur: %v", err)
	}
	if env.RetryCount != 1 {
		t.Errorf("expected retry_count 1 after retry, got %d", env.RetryCount)
	}
	if env.RetryPending || !env.RetryDelivered {
		t.Errorf("completed retry state = pending:%t delivered:%t, want terminal delivery", env.RetryPending, env.RetryDelivered)
	}
}

func TestRetryFromDLQRefusesCompletedEnvelopeAfterDeliveryIsRedLQed(t *testing.T) {
	const (
		agent    = "alice"
		filename = "retry-terminal.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	root := openDeliveryRootForTest(t, rootPath)
	dlqPath := createDLQMessage(t, rootPath, agent, filename, []byte("retry body"))
	dlqFilename := filepath.Base(dlqPath)

	if err := RetryFromDLQ(root, agent, dlqFilename, false); err != nil {
		t.Fatalf("first RetryFromDLQ: %v", err)
	}
	if _, err := MoveToDLQ(root, agent, filename, "retry-terminal-redlq", "parse_error", "still malformed"); err != nil {
		t.Fatalf("consume retried delivery back to DLQ: %v", err)
	}

	err := RetryFromDLQ(root, agent, dlqFilename, true)
	if !errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("late retry of completed envelope = %v, want terminal already-delivered refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(rootPath, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("late retry recreated inbox/new delivery: %v", statErr)
	}
	envelope, _, readErr := ReadDLQEnvelopePath(filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename))
	if readErr != nil {
		t.Fatalf("read completed retry audit: %v", readErr)
	}
	if envelope.RetryCount != 1 {
		t.Fatalf("completed retry count = %d, want 1", envelope.RetryCount)
	}
	if envelope.RetryPending || !envelope.RetryDelivered {
		t.Fatalf(
			"completed retry state = pending:%t delivered:%t, want terminal delivery",
			envelope.RetryPending,
			envelope.RetryDelivered,
		)
	}
}

func TestRetryFromDLQLegacyCompletedRetryIsIndeterminateWithoutDestination(t *testing.T) {
	const (
		agent    = "alice"
		filename = "legacy-completed.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Released v1 wrote this same shape both after a successful delivery and
	// after a known pre-commit delivery failure. A positive count alone is
	// therefore ambiguous and must never be retried blindly.
	raw := []byte("---\n" +
		`{"schema":"amq/dlq/v1","id":"legacy-completed","original_id":"legacy-original","original_file":"legacy-completed.md","failure_reason":"parse_error","failure_detail":"legacy fixture","failure_time":"2026-07-28T00:00:00Z","retry_count":1,"source_dir":"new"}` +
		"\n---\nlegacy body")
	dlqPath := filepath.Join(AgentDLQNew(root, agent), "legacy-completed.md")
	if err := os.WriteFile(dlqPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy DLQ envelope: %v", err)
	}

	err := RetryFromDLQ(openDeliveryRootForTest(t, root), agent, filename, true)
	if !errors.Is(err, ErrDLQRetryIndeterminate) {
		t.Fatalf("legacy ambiguous retry = %v, want indeterminate refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(root, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("legacy indeterminate retry recreated inbox/new delivery: %v", statErr)
	}
	if after, readErr := os.ReadFile(dlqPath); readErr != nil || !bytes.Equal(after, raw) {
		t.Fatalf("legacy indeterminate retry mutated envelope: bytes=%q err=%v", after, readErr)
	}
}

func TestRetryFromDLQLegacyCompletedRetryFinalizesVisibleDestination(t *testing.T) {
	const (
		agent    = "alice"
		filename = "legacy-visible.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	raw := []byte("---\n" +
		`{"schema":"amq/dlq/v1","id":"legacy-visible","original_id":"legacy-original","original_file":"legacy-visible.md","failure_reason":"parse_error","failure_detail":"legacy fixture","failure_time":"2026-07-28T00:00:00Z","retry_count":1,"source_dir":"new"}` +
		"\n---\nlegacy body")
	dlqPath := filepath.Join(AgentDLQNew(root, agent), "legacy-visible.md")
	if err := os.WriteFile(dlqPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy DLQ envelope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, agent), filename), []byte("legacy body"), 0o600); err != nil {
		t.Fatalf("write visible legacy destination: %v", err)
	}

	err := RetryFromDLQ(openDeliveryRootForTest(t, root), agent, filename, true)
	if !errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("legacy visible retry = %v, want terminal delivered result", err)
	}
	curPath := filepath.Join(AgentDLQCur(root, agent), filename)
	env, _, readErr := ReadDLQEnvelopePath(curPath)
	if readErr != nil {
		t.Fatalf("read finalized legacy envelope: %v", readErr)
	}
	if env.RetryState != RetryStateDelivered || env.RetryPending || !env.RetryDelivered {
		t.Fatalf("finalized legacy state = %q pending:%t delivered:%t, want delivered", env.RetryState, env.RetryPending, env.RetryDelivered)
	}
}

func TestRetryFromDLQRefusesRetainedOriginalInCurWithoutMutation(t *testing.T) {
	const (
		agent    = "alice"
		filename = "retained_original.md"
	)
	root := t.TempDir()
	if err := EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	sourcePath := filepath.Join(AgentInboxCur(root, agent), filename)
	content := []byte("retained corrupt payload")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	deliveryRoot := openDeliveryRootForTest(t, root)
	dlqNewDir := filepath.Join("agents", agent, "dlq", "new")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == dlqNewDir {
			return errors.New("injected envelope sync failure")
		}
		return deliveryRoot.syncDirPlatform(dir)
	}
	dlqPath, transitionErr := MoveCurToDLQ(
		deliveryRoot,
		agent,
		filename,
		"retained_original",
		"parse_error",
		"bad data",
	)
	var transition *DLQTransitionError
	if !errors.As(transitionErr, &transition) || !transition.SourceRetained {
		t.Fatalf("fixture transition = %T %v, want retained source", transitionErr, transitionErr)
	}
	deliveryRoot.syncDirForTest = nil

	beforeEnvelope, err := os.ReadFile(dlqPath)
	if err != nil {
		t.Fatalf("read envelope before retry: %v", err)
	}
	dlqFilename := filepath.Base(dlqPath)

	err = RetryFromDLQ(deliveryRoot, agent, dlqFilename, false)
	if err == nil || !strings.Contains(err.Error(), "inbox/cur") {
		t.Fatalf("RetryFromDLQ retained-cur error = %v, want inbox/cur refusal", err)
	}
	afterEnvelope, err := os.ReadFile(dlqPath)
	if err != nil {
		t.Fatalf("read envelope after refused retry: %v", err)
	}
	if string(afterEnvelope) != string(beforeEnvelope) {
		t.Fatal("refused retained-cur retry mutated the DLQ envelope")
	}
	if _, statErr := os.Stat(filepath.Join(AgentDLQCur(root, agent), dlqFilename)); !os.IsNotExist(statErr) {
		t.Fatalf("refused retry moved envelope to dlq/cur: %v", statErr)
	}
	if got, readErr := os.ReadFile(sourcePath); readErr != nil || string(got) != string(content) {
		t.Fatalf("retained source after retry = %q, err=%v; want %q", got, readErr, content)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(root, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("refused retry redelivered original to inbox/new: %v", statErr)
	}
}

func TestReadDLQEnvelopeRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	dlqPath := createDLQMessage(t, root, "alice", "symlink_source.md", []byte("test content"))
	link := filepath.Join(AgentDLQNew(root, "alice"), "symlink_dlq.md")
	if err := os.Symlink(dlqPath, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, err := ReadDLQEnvelopePath(link)
	if err == nil {
		t.Fatal("expected symlink DLQ envelope to be rejected")
	}
}

func TestMoveCurToDLQRejectsSymlinkSource(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("target content"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(AgentInboxCur(root, "alice"), "symlink_source.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := MoveCurToDLQ(openDeliveryRootForTest(t, root), "alice", "symlink_source.md", "symlink_source", "parse_error", "test")
	if err == nil {
		t.Fatal("expected symlink inbox source to be rejected")
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("target should remain untouched: %v", statErr)
	}
}

func TestRetryFromDLQRejectsTraversalOriginalFile(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "safe_msg.md", []byte("test content"))
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("ReadDLQEnvelope: %v", err)
	}
	env.OriginalFile = "../escape.md"
	data, err := serializeDLQMessage(*env, body)
	if err != nil {
		t.Fatalf("serialize tampered envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("write tampered envelope: %v", err)
	}

	err = RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false)
	if err == nil {
		t.Fatal("expected traversal original_file to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid original_file") {
		t.Fatalf("expected invalid original_file error, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "alice", "inbox", "escape.md")); !os.IsNotExist(err) {
		t.Fatalf("retry should not create escaped inbox file, stat err: %v", err)
	}
}

func TestRetryFromDLQEnvelopeUpdateFailureReturnsErrorBeforeRedelivery(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "update_failure.md", []byte("test content"))
	dlqCur := AgentDLQCur(root, "alice")
	if err := os.RemoveAll(dlqCur); err != nil {
		t.Fatalf("remove dlq cur: %v", err)
	}
	if err := os.WriteFile(dlqCur, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block dlq cur: %v", err)
	}

	err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false)
	if err == nil {
		t.Fatal("expected RetryFromDLQ to return envelope update error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected blocked dlq/cur error, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxNew(root, "alice"), "update_failure.md")); !os.IsNotExist(err) {
		t.Fatalf("retry should not redeliver before envelope update succeeds, stat err: %v", err)
	}
}

func TestRetryFromDLQRedeliveryFailureReturnsError(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	dlqPath := createDLQMessage(t, root, "alice", "redelivery_failure.md", []byte("test content"))
	inboxTmp := AgentInboxTmp(root, "alice")
	if err := os.RemoveAll(inboxTmp); err != nil {
		t.Fatalf("remove inbox tmp: %v", err)
	}
	if err := os.WriteFile(inboxTmp, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block inbox tmp: %v", err)
	}

	err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false)
	if err == nil {
		t.Fatal("expected RetryFromDLQ to return redelivery error")
	}
	if !strings.Contains(err.Error(), "redeliver to inbox") {
		t.Fatalf("expected redelivery error, got: %v", err)
	}

	curPath := filepath.Join(AgentDLQCur(root, "alice"), filepath.Base(dlqPath))
	env, _, err := ReadDLQEnvelopePath(curPath)
	if err != nil {
		t.Fatalf("expected updated DLQ envelope in cur: %v", err)
	}
	if env.RetryCount != 1 {
		t.Fatalf("expected retry_count 1 after state transition, got %d", env.RetryCount)
	}
	if env.RetryPending || env.RetryDelivered {
		t.Fatalf(
			"failed retry state = pending:%t delivered:%t, want reset nonterminal attempt",
			env.RetryPending,
			env.RetryDelivered,
		)
	}

	if err := os.Remove(inboxTmp); err != nil {
		t.Fatalf("remove inbox tmp blocker: %v", err)
	}
	if err := os.MkdirAll(inboxTmp, 0o700); err != nil {
		t.Fatalf("restore inbox tmp: %v", err)
	}
	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", filepath.Base(dlqPath), false); err != nil {
		t.Fatalf("retry after known pre-commit failure: %v", err)
	}
	env, _, err = ReadDLQEnvelopePath(curPath)
	if err != nil {
		t.Fatalf("read completed retry audit: %v", err)
	}
	if env.RetryCount != 2 || env.RetryPending || !env.RetryDelivered {
		t.Fatalf(
			"completed retry after reset = count:%d pending:%t delivered:%t, want count 2 terminal",
			env.RetryCount,
			env.RetryPending,
			env.RetryDelivered,
		)
	}
}

func TestRetryFromDLQMaxRetries(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a message and move to DLQ with retry_count = 3
	inboxNew := AgentInboxNew(root, "alice")
	filename := "test_msg.md"
	content := []byte("test content")
	if err := os.WriteFile(filepath.Join(inboxNew, filename), content, 0o600); err != nil {
		t.Fatalf("write test msg: %v", err)
	}

	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "test_msg", "test_failure", "test")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}

	// Manually set retry_count to MaxRetries
	env, body, _ := ReadDLQEnvelopePath(dlqPath)
	env.RetryCount = MaxRetries
	data, _ := serializeDLQMessage(*env, body)
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("update DLQ: %v", err)
	}

	dlqFilename := filepath.Base(dlqPath)

	// Retry should fail without --force
	err = RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, false)
	if err == nil {
		t.Errorf("expected error due to max retries")
	}
	if !strings.Contains(err.Error(), "max retries") {
		t.Errorf("expected 'max retries' error, got: %v", err)
	}

	// Retry with --force should succeed
	if err := RetryFromDLQ(openDeliveryRootForTest(t, root), "alice", dlqFilename, true); err != nil {
		t.Fatalf("RetryFromDLQ with force: %v", err)
	}
}

func createDLQMessage(t *testing.T, root, agent, filename string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, agent), filename), content, 0o600); err != nil {
		t.Fatalf("write source message: %v", err)
	}
	dlqPath, err := MoveToDLQ(openDeliveryRootForTest(t, root), agent, filename, strings.TrimSuffix(filename, ".md"), "test_failure", "test detail")
	if err != nil {
		t.Fatalf("MoveToDLQ: %v", err)
	}
	return dlqPath
}

func TestFindDLQMessage(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create DLQ message in new
	dlqNew := AgentDLQNew(root, "alice")
	filename := "dlq_test.md"
	if err := os.WriteFile(filepath.Join(dlqNew, filename), []byte("test"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Find in new
	path, box, err := FindDLQMessage(openDeliveryRootForTest(t, root), "alice", filename)
	if err != nil {
		t.Fatalf("FindDLQMessage: %v", err)
	}
	if box != BoxNew {
		t.Errorf("expected box 'new', got %s", box)
	}
	if !strings.HasSuffix(path, filename) {
		t.Errorf("path should end with filename")
	}

	// Move to cur
	if err := MoveDLQNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err != nil {
		t.Fatalf("MoveDLQNewToCur: %v", err)
	}

	// Find in cur
	_, box, err = FindDLQMessage(openDeliveryRootForTest(t, root), "alice", filename)
	if err != nil {
		t.Fatalf("FindDLQMessage after move: %v", err)
	}
	if box != BoxCur {
		t.Errorf("expected box 'cur', got %s", box)
	}
}

func TestDivergentDLQBoxesKeepCurAuthoritativeAndSelfHeal(t *testing.T) {
	const (
		agent    = "alice"
		filename = "divergent-boxes.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	newPath := createDLQMessage(t, rootPath, agent, filename, []byte("stale new body"))
	dlqFilename := filepath.Base(newPath)
	staleNew, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read stale new envelope: %v", err)
	}
	staleEnv, _, err := ReadDLQEnvelopePath(newPath)
	if err != nil {
		t.Fatalf("parse stale new envelope: %v", err)
	}
	staleEnv.RetryCount = 2
	setRetryState(staleEnv, RetryStateDelivered)
	curData, err := serializeDLQMessage(*staleEnv, []byte("authoritative cur body"))
	if err != nil {
		t.Fatalf("serialize authoritative cur envelope: %v", err)
	}
	curPath := filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename)
	if err := os.WriteFile(curPath, curData, 0o600); err != nil {
		t.Fatalf("write authoritative cur envelope: %v", err)
	}

	root := openDeliveryRootForTest(t, rootPath)
	path, box, err := FindDLQMessage(root, agent, dlqFilename)
	if err != nil || path != filepath.Join("agents", agent, "dlq", BoxCur, dlqFilename) || box != BoxCur {
		t.Fatalf("FindDLQMessage divergent boxes = (%q,%q,%v), want cur authority", path, box, err)
	}

	inspected, body, inspectedBox, err := InspectDLQEnvelope(root, agent, dlqFilename)
	if err != nil {
		t.Fatalf("InspectDLQEnvelope divergent boxes: %v", err)
	}
	if inspectedBox != BoxCur || inspected.RetryState != RetryStateDelivered || !inspected.RetryDelivered || string(body) != "authoritative cur body" {
		t.Fatalf("inspection = box:%q state:%q delivered:%t body:%q, want authoritative cur", inspectedBox, inspected.RetryState, inspected.RetryDelivered, body)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("inspect did not reconcile stale dlq/new copy: %v", err)
	}
	currentCur, err := os.ReadFile(curPath)
	if err != nil {
		t.Fatalf("read authoritative cur after inspect: %v", err)
	}
	if !bytes.Equal(currentCur, curData) {
		t.Fatalf("inspect overwrote authoritative cur:\n got %q\nwant %q", currentCur, curData)
	}
	// Recreate the crash residue after the read path healed it. This makes the
	// retry assertion causal: retry itself must select cur, remove stale new,
	// and never resurrect the old ready envelope.
	if err := os.WriteFile(newPath, staleNew, 0o600); err != nil {
		t.Fatalf("recreate stale new retry residue: %v", err)
	}

	err = RetryFromDLQ(root, agent, dlqFilename, true)
	if !errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("retry authoritative terminal envelope = %v, want delivered refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(AgentInboxNew(rootPath, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("retry stale-new state recreated inbox/new delivery: %v", statErr)
	}
	if afterRetry, readErr := os.ReadFile(curPath); readErr != nil || !bytes.Equal(afterRetry, curData) {
		t.Fatalf("retry changed authoritative cur: bytes=%q err=%v", afterRetry, readErr)
	}
	if err := os.WriteFile(newPath, staleNew, 0o600); err != nil {
		t.Fatalf("recreate stale new direct-move residue: %v", err)
	}
	if err := MoveDLQNewToCur(root, agent, dlqFilename); err != nil {
		t.Fatalf("direct move reconciliation: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("direct move did not remove stale dlq/new copy: %v", err)
	}
	if afterMove, readErr := os.ReadFile(curPath); readErr != nil || !bytes.Equal(afterMove, curData) {
		t.Fatalf("direct move overwrote authoritative cur: bytes=%q err=%v", afterMove, readErr)
	}
	if bytes.Equal(staleNew, curData) {
		t.Fatal("fixture must have divergent new and cur contents")
	}
}

func TestParseDLQMessageRejectsInvalidExplicitRetryState(t *testing.T) {
	for name, test := range map[string]struct {
		header string
		want   string
	}{
		"unknown":                       {header: `{"retry_state":"lost"}`, want: "retry_state"},
		"inconsistent":                  {header: `{"retry_state":"ready","retry_pending":true}`, want: "retry_state"},
		"negative count":                {header: `{"retry_count":-1}`, want: "retry_count"},
		"pending without attempt":       {header: `{"retry_state":"pending","retry_pending":true}`, want: "retry_count"},
		"delivered without attempt":     {header: `{"retry_state":"delivered","retry_delivered":true}`, want: "retry_count"},
		"indeterminate without attempt": {header: `{"retry_state":"indeterminate"}`, want: "retry_count"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseDLQMessage([]byte("---\n" + test.header + "\n---\nbody"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse invalid retry state = %v, want fail-closed state error", err)
			}
		})
	}
}

func TestMoveDLQNewToCurRejectsExpiredPinnedBatchWithoutMutation(t *testing.T) {
	const (
		agent    = "alice"
		filename = "expired-batch.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	before := []byte("retained DLQ envelope")
	newPath := filepath.Join(AgentDLQNew(rootPath, agent), filename)
	if err := os.WriteFile(newPath, before, 0o600); err != nil {
		t.Fatalf("write DLQ fixture: %v", err)
	}

	root := openDeliveryRootForTest(t, rootPath)
	var retained *DeliveryRoot
	if err := root.WithPinnedBatch(func(batch *DeliveryRoot) error {
		retained = batch
		return nil
	}); err != nil {
		t.Fatalf("WithPinnedBatch: %v", err)
	}

	err := MoveDLQNewToCur(retained, agent, filename)
	if err == nil || err.Error() != "pinned delivery batch expired" {
		t.Fatalf("MoveDLQNewToCur expired batch error = %v", err)
	}
	after, readErr := os.ReadFile(newPath)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("DLQ new after expired move = %q, err=%v; want unchanged", after, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(AgentDLQCur(rootPath, agent), filename)); !os.IsNotExist(statErr) {
		t.Fatalf("expired move created dlq/cur artifact: %v", statErr)
	}
}

func TestMoveDLQNewToCurPostRenameSyncFailureReportsCommittedMove(t *testing.T) {
	const (
		agent    = "alice"
		filename = "committed-inspection.md"
	)

	for _, test := range []struct {
		name    string
		failNew bool
		failCur bool
	}{
		{name: "dlq new", failNew: true},
		{name: "dlq cur", failCur: true},
		{name: "both directories", failNew: true, failCur: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			if err := EnsureAgentDirs(rootPath, agent); err != nil {
				t.Fatalf("EnsureAgentDirs: %v", err)
			}
			before := []byte("committed DLQ envelope")
			newPath := filepath.Join(AgentDLQNew(rootPath, agent), filename)
			if err := os.WriteFile(newPath, before, 0o600); err != nil {
				t.Fatalf("write DLQ fixture: %v", err)
			}

			root := openDeliveryRootForTest(t, rootPath)
			newErr := errors.New("injected dlq/new sync failure")
			curErr := errors.New("injected dlq/cur sync failure")
			syncCalls := make(map[string]int)
			newDir := filepath.Join("agents", agent, "dlq", "new")
			curDir := filepath.Join("agents", agent, "dlq", "cur")
			root.syncDirForTest = func(dir string) error {
				syncCalls[dir]++
				switch {
				case dir == newDir && test.failNew:
					return newErr
				case dir == curDir && test.failCur:
					return curErr
				default:
					return root.syncDirPlatform(dir)
				}
			}

			err := MoveDLQNewToCur(root, agent, filename)
			var committed *CommittedDurabilityError
			if !errors.As(err, &committed) {
				t.Fatalf("MoveDLQNewToCur error = %T %v, want CommittedDurabilityError", err, err)
			}
			if test.failNew && !errors.Is(err, newErr) {
				t.Fatalf("MoveDLQNewToCur error = %v, want dlq/new sync failure", err)
			}
			if test.failCur && !errors.Is(err, curErr) {
				t.Fatalf("MoveDLQNewToCur error = %v, want dlq/cur sync failure", err)
			}
			wantCur := filepath.Join(AgentDLQCur(rootPath, agent), filename)
			if committed.FinalPath != wantCur || committed.Recipient != agent {
				t.Fatalf("committed move = %#v, want path %q recipient %q", committed, wantCur, agent)
			}
			if syncCalls[newDir] != 1 || syncCalls[curDir] != 1 {
				t.Fatalf("sync calls = new:%d cur:%d, want one attempt each", syncCalls[newDir], syncCalls[curDir])
			}
			if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
				t.Fatalf("committed envelope remains in dlq/new: %v", statErr)
			}
			got, readErr := os.ReadFile(wantCur)
			if readErr != nil || !bytes.Equal(got, before) {
				t.Fatalf("committed envelope in dlq/cur = %q, err=%v", got, readErr)
			}
		})
	}
}

func TestRetryFromDLQRedeliveryPostRenameSyncFailureReportsCommittedInboxResult(t *testing.T) {
	const (
		agent    = "alice"
		filename = "committed-retry.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	content := []byte("committed retry payload")
	dlqPath := createDLQMessage(t, rootPath, agent, filename, content)
	dlqFilename := filepath.Base(dlqPath)

	root := openDeliveryRootForTest(t, rootPath)
	injectedErr := errors.New("injected retry inbox/new sync failure")
	root.syncDirForTest = func(dir string) error {
		if dir == filepath.Join("agents", agent, "inbox", "new") {
			return injectedErr
		}
		return root.syncDirPlatform(dir)
	}

	err := RetryFromDLQ(root, agent, dlqFilename, false)
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, injectedErr) {
		t.Fatalf("RetryFromDLQ error = %T %v, want committed inbox result", err, err)
	}
	wantFinal := filepath.Join(AgentInboxNew(rootPath, agent), filename)
	if committed.FinalPath != wantFinal || committed.Recipient != agent {
		t.Fatalf("committed retry metadata = %#v, want %q", committed, wantFinal)
	}
	got, readErr := os.ReadFile(wantFinal)
	if readErr != nil || !bytes.Equal(got, content) {
		t.Fatalf("committed retry payload = %q, err=%v", got, readErr)
	}
	if _, statErr := os.Stat(dlqPath); !os.IsNotExist(statErr) {
		t.Fatalf("committed retry envelope remains in dlq/new: %v", statErr)
	}
	env, _, readErr := ReadDLQEnvelopePath(filepath.Join(AgentDLQCur(rootPath, agent), dlqFilename))
	if readErr != nil || env.RetryCount != 1 || env.RetryPending || !env.RetryDelivered {
		t.Fatalf("committed retry cur envelope = %#v, err=%v; want completed retry audit", env, readErr)
	}
}
