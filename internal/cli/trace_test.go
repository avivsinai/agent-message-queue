package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
)

type traceResultJSON struct {
	Schema    string                  `json:"schema"`
	MessageID string                  `json:"message_id"`
	Status    string                  `json:"status"`
	Legs      map[string]traceLegJSON `json:"legs"`
}

func TestTraceJoinsWrittenAndIndeterminateNotificationAttempts(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeTraceNotificationJournal(t, root, "codex",
		notificationattempt.Record{
			Schema:     notificationattempt.SchemaVersion,
			AttemptID:  "attempt-written",
			Phase:      notificationattempt.PhasePrepared,
			MessageIDs: []string{"msg-notification"},
			Agent:      "codex",
			Mode:       "raw",
			RecordedAt: "2026-09-01T08:00:00Z",
		},
		notificationattempt.Record{
			Schema:     notificationattempt.SchemaVersion,
			AttemptID:  "attempt-written",
			Phase:      notificationattempt.PhaseResult,
			MessageIDs: []string{"msg-notification"},
			Agent:      "codex",
			Mode:       "raw",
			RecordedAt: "2026-09-01T08:00:01Z",
			Outcome:    notificationattempt.OutcomeWritten,
			Detail:     "terminal bytes accepted",
		},
		notificationattempt.Record{
			Schema:     notificationattempt.SchemaVersion,
			AttemptID:  "attempt-indeterminate",
			Phase:      notificationattempt.PhasePrepared,
			MessageIDs: []string{"msg-notification"},
			Agent:      "codex",
			Mode:       "external",
			RecordedAt: "2026-09-01T08:00:02Z",
		},
	)

	result := collectTrace(root, "msg-notification")
	leg := result.Legs["notification"]
	if leg.Status != "evidence" || leg.State != traceNotificationRecordPresent || len(leg.Evidence) != 2 {
		t.Fatalf("notification leg = %+v", leg)
	}
	states := map[string]bool{}
	for _, evidence := range leg.Evidence {
		states[evidence.State] = true
		if evidence.Authority != "notification_attempt" || evidence.Notification == nil {
			t.Fatalf("notification evidence = %+v", evidence)
		}
		if strings.Contains(evidence.Limitation, "seen") && evidence.State == notificationattempt.StateIndeterminate {
			t.Fatalf("indeterminate limitation should explain missing result: %+v", evidence)
		}
	}
	if !states[notificationattempt.OutcomeWritten] || !states[notificationattempt.StateIndeterminate] {
		t.Fatalf("notification states = %v", states)
	}
}

type traceLegJSON struct {
	Status   string            `json:"status"`
	State    string            `json:"state"`
	Evidence []json.RawMessage `json:"evidence"`
	Detail   string            `json:"detail"`
	NextStep string            `json:"next_step"`
}

func TestTraceCommandEntryPointJoinsCurrentEvidence(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")

	sent := runSendJSONForTest(t,
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--subject", "trace target",
		"--body", "please trace this",
		"--json",
	)
	messageID, _ := sent["id"].(string)
	if messageID == "" {
		t.Fatalf("send result has no id: %#v", sent)
	}
	if _, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", root,
			"--me", "bob",
			"--id", messageID,
			"--body", "reply evidence",
			"--json",
		})
	}); err != nil {
		t.Fatalf("runReply: %v", err)
	}

	if _, err := deliverToInboxForTest(t, root, "alice", messageID+".md", []byte("malformed trace fixture")); err != nil {
		t.Fatalf("deliver malformed message: %v", err)
	}
	drainCmd := exec.Command(os.Args[0], "drain", "--me", "alice", "--root", root, "--json")
	drainCmd.Env = append(os.Environ(), cliHelperEnv+"=1", "AMQ_NO_UPDATE_CHECK=1")
	if output, err := drainCmd.CombinedOutput(); err != nil {
		t.Fatalf("drain malformed message: %v\noutput: %s", err, output)
	}

	cmd := exec.Command(os.Args[0], "trace", messageID, "--root", root, "--json")
	cmd.Env = append(os.Environ(), cliHelperEnv+"=1", "AMQ_NO_UPDATE_CHECK=1")
	stdout, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("trace command failed: %v\nstderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("trace command failed: %v", err)
	}

	var result traceResultJSON
	if err := json.Unmarshal(stdout, &result); err != nil {
		t.Fatalf("unmarshal trace JSON: %v\nstdout: %s", err, stdout)
	}
	if result.Schema != "amq/trace/v1" || result.MessageID != messageID || result.Status != "found" {
		t.Fatalf("trace identity/status = %#v", result)
	}
	for _, name := range []string{"message", "route", "delivery", "dlq", "receipts", "thread"} {
		leg := result.Legs[name]
		if leg.Status != "evidence" || len(leg.Evidence) == 0 {
			t.Fatalf("%s leg = %#v", name, leg)
		}
	}
	deliveryEvidence := decodeTraceEvidence(t, result.Legs["delivery"].Evidence[0])
	if deliveryEvidence["authority"] == "" || deliveryEvidence["durability"] != "no_evidence" {
		t.Fatalf("delivery evidence contract = %#v", deliveryEvidence)
	}
	dlqEvidence := decodeTraceEvidence(t, result.Legs["dlq"].Evidence[0])
	dlq, _ := dlqEvidence["dlq"].(map[string]any)
	if dlq["original_id"] != messageID {
		t.Fatalf("dlq evidence contract = %#v", dlqEvidence)
	}
	receiptEvidence := decodeTraceEvidence(t, result.Legs["receipts"].Evidence[0])
	receiptRecord, _ := receiptEvidence["receipt"].(map[string]any)
	if receiptRecord["msg_id"] != messageID || receiptRecord["stage"] == "" {
		t.Fatalf("receipt evidence contract = %#v", receiptEvidence)
	}
	threadEvidence := decodeTraceEvidence(t, result.Legs["thread"].Evidence[0])
	relation, _ := threadEvidence["relation"].(map[string]any)
	if relation["message_id"] == "" || relation["relation"] == "" {
		t.Fatalf("thread evidence contract = %#v", threadEvidence)
	}
	notification := result.Legs["notification"]
	if notification.Status != "no_evidence" || notification.NextStep == "" {
		t.Fatalf("notification leg = %#v", notification)
	}
	if notification.State != traceNotificationLedgerAbsent || notification.Detail != "no notification attempt ledger file was found" {
		t.Fatalf("notification absent state = %#v", notification)
	}

	textOutput, _ := captureOutput(t, func() error {
		return runTrace([]string{messageID, "--root", root})
	})
	for _, want := range []string{"parse_error", "dlq by alice", "references_target", "durability no_evidence"} {
		if !strings.Contains(textOutput, want) {
			t.Fatalf("trace text missing %q:\n%s", want, textOutput)
		}
	}
}

func TestTraceNotificationLedgerPresentWithoutMessageUsesPlatformPath(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatal(err)
	}
	writeTraceNotificationJournal(t, root, "bob", notificationattempt.Record{
		Schema:     notificationattempt.SchemaVersion,
		AttemptID:  "attempt-other-message",
		Phase:      notificationattempt.PhasePrepared,
		MessageIDs: []string{"other-message"},
		Agent:      "bob",
		Mode:       "raw",
		RecordedAt: "2026-09-01T08:10:00Z",
	})

	result := collectTrace(root, "target-message")
	leg := result.Legs["notification"]
	if leg.Status != "no_evidence" || leg.State != traceNotificationLedgerPresentNoRecord {
		t.Fatalf("notification leg = %#v, want present ledger without target record", leg)
	}
	if leg.Detail != "notification attempt ledger exists, but no record for this message was found" {
		t.Fatalf("notification detail = %q", leg.Detail)
	}

	textOutput, _ := captureOutput(t, func() error {
		return runTrace([]string{"target-message", "--root", root})
	})
	if !strings.Contains(textOutput, "notification no_evidence [ledger_present_no_record]") {
		t.Fatalf("trace text did not expose ledger state:\n%s", textOutput)
	}
}

func TestTraceNotificationLedgerPresenceIncludesRotatedJournal(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "bob"); err != nil {
		t.Fatal(err)
	}
	writeTraceNotificationJournalNamed(t, root, "bob", notificationattempt.LogFilename+notificationattempt.RotatedSuffix, notificationattempt.Record{
		Schema:     notificationattempt.SchemaVersion,
		AttemptID:  "attempt-rotated-other-message",
		Phase:      notificationattempt.PhasePrepared,
		MessageIDs: []string{"other-message"},
		Agent:      "bob",
		Mode:       "raw",
		RecordedAt: "2026-09-01T08:20:00Z",
	})

	leg := collectTrace(root, "target-message").Legs["notification"]
	if leg.State != traceNotificationLedgerPresentNoRecord {
		t.Fatalf("notification state = %q, want rotated ledger present/no record", leg.State)
	}
}

func TestNotificationAttemptTraceStatePrecedence(t *testing.T) {
	tests := []struct {
		name          string
		ledgerPresent bool
		recordPresent bool
		supported     bool
		want          string
	}{
		{name: "absent", want: traceNotificationLedgerAbsent, supported: true},
		{name: "present without record", ledgerPresent: true, want: traceNotificationLedgerPresentNoRecord, supported: true},
		{name: "record present", ledgerPresent: true, recordPresent: true, want: traceNotificationRecordPresent, supported: true},
		{name: "unsupported without journal", want: traceNotificationLedgerUnsupported},
		{name: "unsupported with existing journal", ledgerPresent: true, want: traceNotificationLedgerPresentNoRecord},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notificationAttemptTraceState(tt.ledgerPresent, tt.recordPresent, tt.supported); got != tt.want {
				t.Fatalf("notificationAttemptTraceState(%v, %v, %v) = %q, want %q", tt.ledgerPresent, tt.recordPresent, tt.supported, got, tt.want)
			}
		})
	}
}

func TestTraceMissingMessageEmitsContractAndNotFoundExit(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}

	stdout, _ := captureOutput(t, func() error {
		err := runTrace([]string{"missing-message", "--root", root, "--json"})
		if err == nil || GetExitCode(err) != ExitNotFound {
			t.Fatalf("trace missing error = %v, exit = %d", err, GetExitCode(err))
		}
		return nil
	})

	var result traceResultJSON
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal trace JSON: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "not_found" {
		t.Fatalf("status = %q, want not_found", result.Status)
	}
	for name, leg := range result.Legs {
		if leg.Status != "no_evidence" {
			t.Fatalf("%s status = %q, want no_evidence", name, leg.Status)
		}
		if leg.NextStep == "" {
			t.Fatalf("%s no_evidence leg has no next_step", name)
		}
	}
}

func TestTraceOutboxCopyDoesNotEstablishDelivery(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	sent := runSendJSONForTest(t,
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "outbox is not delivery proof",
		"--json",
	)
	messageID, _ := sent["id"].(string)
	if err := os.Remove(filepath.Join(fsq.AgentInboxNew(root, "bob"), messageID+".md")); err != nil {
		t.Fatal(err)
	}

	stdout, _ := captureOutput(t, func() error {
		return runTrace([]string{messageID, "--root", root, "--json"})
	})
	var result traceResultJSON
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal trace JSON: %v\nstdout: %s", err, stdout)
	}
	if result.Status != "found" {
		t.Fatalf("status = %q, want found from message evidence", result.Status)
	}
	if leg := result.Legs["message"]; leg.Status != "evidence" || len(leg.Evidence) == 0 {
		t.Fatalf("message leg = %#v", leg)
	}
	delivery := result.Legs["delivery"]
	if delivery.Status != "no_evidence" || delivery.NextStep == "" {
		t.Fatalf("outbox incorrectly established delivery: %#v", delivery)
	}
}

func TestTraceInboxCurWithoutReceiptDoesNotInferDrain(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	sent := runSendJSONForTest(t,
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "legacy cur fixture",
		"--json",
	)
	messageID, _ := sent["id"].(string)
	if err := fsq.MoveNewToCur(openDeliveryRootForCLITest(t, root), "bob", messageID+".md"); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}

	stdout, _ := captureOutput(t, func() error {
		return runTrace([]string{messageID, "--root", root, "--json"})
	})
	var result traceResultJSON
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal trace JSON: %v\nstdout: %s", err, stdout)
	}
	if delivery := result.Legs["delivery"]; delivery.Status != "evidence" || len(delivery.Evidence) != 1 {
		t.Fatalf("delivery leg = %#v", delivery)
	}
	receiptLeg := result.Legs["receipts"]
	if receiptLeg.Status != "no_evidence" || receiptLeg.NextStep == "" {
		t.Fatalf("receipt leg inferred a drain: %#v", receiptLeg)
	}

	textOutput, _ := captureOutput(t, func() error {
		return runTrace([]string{messageID, "--root", root})
	})
	if strings.Contains(textOutput, "drained by") {
		t.Fatalf("trace inferred drain from inbox/cur:\n%s", textOutput)
	}
}

func TestTraceDegradesOnIncompleteMailboxLayout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents", "orphan"), 0o700); err != nil {
		t.Fatal(err)
	}

	result := collectTrace(root, "absent")
	for _, name := range []string{"message", "dlq", "receipts", "thread"} {
		leg := result.Legs[name]
		if leg.Status != "error" || leg.Detail == "" || leg.NextStep == "" {
			t.Fatalf("%s leg hid missing mailbox components: %#v", name, leg)
		}
	}

	stdout, _ := captureOutput(t, func() error {
		err := runTrace([]string{"absent", "--root", root})
		if err == nil || GetExitCode(err) != ExitNotFound {
			t.Fatalf("trace incomplete root error = %v, exit = %d", err, GetExitCode(err))
		}
		return nil
	})
	for _, want := range []string{"Trace absent: not_found", "no_evidence", "next:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("trace text missing %q:\n%s", want, stdout)
		}
	}
}

func TestTraceDoesNotFollowAgentsSymlinkOutsidePinnedRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(outside, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, outside, "alice", "bob")
	sent := runSendJSONForTest(t,
		"--root", outside,
		"--me", "alice",
		"--to", "bob",
		"--body", "must stay outside",
		"--json",
	)
	messageID, _ := sent["id"].(string)
	if err := os.Symlink(filepath.Join(outside, "agents"), filepath.Join(root, "agents")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result := collectTrace(root, messageID)
	if result.Status != "not_found" {
		t.Fatalf("status = %q, want not_found", result.Status)
	}
	message := result.Legs["message"]
	if message.Status != "error" || len(message.Evidence) != 0 || message.NextStep == "" {
		t.Fatalf("message leg followed outside root or hid error: %#v", message)
	}
}

func TestTraceReportsUnreadableJoinInputsInsteadOfNoEvidence(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	sent := runSendJSONForTest(t,
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "valid target",
		"--json",
	)
	messageID, _ := sent["id"].(string)
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	for _, dir := range []string{
		filepath.Join("agents", "bob", "inbox", "new"),
		filepath.Join("agents", "bob", "dlq", "new"),
	} {
		if _, err := deliveryRoot.WriteFileAtomic(dir, "broken.md", []byte("not an envelope"), 0o600); err != nil {
			t.Fatalf("write corrupt join input: %v", err)
		}
	}

	result := collectTrace(root, messageID)
	if result.Status != "partial" {
		t.Fatalf("status = %q, want partial", result.Status)
	}
	for _, name := range []string{"thread", "dlq"} {
		leg := result.Legs[name]
		if leg.Status != "error" || leg.NextStep == "" {
			t.Fatalf("%s leg hid unreadable input: %#v", name, leg)
		}
	}
}

func decodeTraceEvidence(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var evidence map[string]any
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatalf("unmarshal trace evidence: %v\nraw: %s", err, raw)
	}
	return evidence
}

func writeTraceNotificationJournal(t testing.TB, root, agent string, records ...notificationattempt.Record) {
	t.Helper()
	writeTraceNotificationJournalNamed(t, root, agent, notificationattempt.LogFilename, records...)
}

func writeTraceNotificationJournalNamed(t testing.TB, root, agent, filename string, records ...notificationattempt.Record) {
	t.Helper()
	var data []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal notification attempt record: %v", err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	if _, err := deliveryRoot.WriteFileAtomic(
		filepath.Join("agents", agent, "receipts"),
		filename,
		data,
		0o600,
	); err != nil {
		t.Fatalf("write notification attempt journal: %v", err)
	}
}
