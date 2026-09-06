package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Restores for the 2026-09-06 cull regression gate: one test per lost
// user-visible DLQ contract (inspect audit, committed-claim recovery sync).

func TestInspectDLQEnvelopeReturnsCurrentAudit(t *testing.T) {
	const (
		agent    = "alice"
		filename = "inspect-audit.md"
	)
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	content := []byte("body")
	dlqPath := createDLQMessage(t, rootPath, agent, filename, content)

	envelope, body, box, err := InspectDLQEnvelope(openDeliveryRootForTest(t, rootPath), agent, filepath.Base(dlqPath))
	if err != nil {
		t.Fatalf("InspectDLQEnvelope: %v", err)
	}
	if box != BoxCur || envelope == nil || envelope.RetryCount != 0 || envelope.RetryPending || envelope.RetryDelivered {
		t.Fatalf("inspection = box:%q envelope:%#v, want fresh cur audit", box, envelope)
	}
	if envelope.OriginalFile != filename {
		t.Fatalf("OriginalFile = %q, want %q", envelope.OriginalFile, filename)
	}
	if string(body) != string(content) {
		t.Fatalf("inspection body = %q, want %q", body, content)
	}
}

func TestMoveToDLQRecoversOneShotCommittedClaimSyncFailure(t *testing.T) {
	const (
		agent    = "alice"
		filename = "recover_claim.md"
	)
	content := []byte("corrupt payload")
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
		if faults == 0 && (dir == filepath.Join("agents", agent, "inbox", "new") ||
			dir == filepath.Join("agents", agent, "inbox", "cur")) {
			faults++
			return injectedErr
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	dlqPath, err := MoveToDLQ(deliveryRoot, agent, filename, "recover_claim", "parse_error", "bad payload")
	if err != nil {
		t.Fatalf("MoveToDLQ after transient committed claim: %v", err)
	}
	if faults != 1 {
		t.Fatalf("injected claim sync faults = %d, want 1", faults)
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
}

func TestRetryFromDLQPendingRetainedTmpCompletesAfterByteCompare(t *testing.T) {
	rootPath := t.TempDir()
	if err := EnsureAgentDirs(rootPath, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	const filename = "retained-tmp.md"
	content := []byte("original body")
	dlqPath := createDLQMessage(t, rootPath, "alice", filename, content)
	env, body, err := ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	env.RetryCount = 1
	setRetryState(env, RetryStatePending)
	data, err := serializeDLQMessage(*env, body)
	if err != nil {
		t.Fatalf("serialize pending envelope: %v", err)
	}
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatalf("write pending envelope: %v", err)
	}

	tmpDir := AgentInboxTmp(rootPath, "alice")
	mismatch := filepath.Join(tmpDir, "."+filename+".tmp-mismatch")
	match := filepath.Join(tmpDir, "."+filename+".tmp-retained")
	if err := os.WriteFile(mismatch, []byte("wrong bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(match, content, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RetryFromDLQ(openDeliveryRootForTest(t, rootPath), "alice", filepath.Base(dlqPath), false); !errors.Is(err, ErrDLQRetryDelivered) {
		t.Fatalf("pending retained tmp = %v, want terminal delivered result", err)
	}
	got, readErr := os.ReadFile(filepath.Join(AgentInboxNew(rootPath, "alice"), filename))
	if readErr != nil || string(got) != string(content) {
		t.Fatalf("recovered new/ = %q, %v", got, readErr)
	}
	if _, statErr := os.Stat(match); !os.IsNotExist(statErr) {
		t.Fatalf("matching tmp still present after completion: %v", statErr)
	}
	got, readErr = os.ReadFile(mismatch)
	if readErr != nil || string(got) != "wrong bytes" {
		t.Fatalf("mismatched tmp = %q, %v; byte-compare must leave it", got, readErr)
	}
}

func TestDLQTransitionErrorMessageGuidesResolution(t *testing.T) {
	cause := errors.New("injected durability failure")
	err := &DLQTransitionError{
		EnvelopePath:   "/root/agents/alice/dlq/new/e.md",
		SourcePath:     "/root/agents/alice/inbox/cur/e.md",
		SourceRetained: true,
		Err:            cause,
	}
	msg := err.Error()
	if !strings.Contains(msg, "resolve the partial transition before retrying") ||
		!strings.Contains(msg, cause.Error()) {
		t.Fatalf("DLQTransitionError message = %q, want resolution guidance with cause", msg)
	}
	if !errors.Is(err, cause) {
		t.Fatal("DLQTransitionError does not unwrap to its cause")
	}
}
