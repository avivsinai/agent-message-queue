package notificationattempt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterPersistsPreparedAndResultInAgentNamespace(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	writer.now = func() time.Time {
		return time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	}

	prepared, err := writer.Prepare([]string{"msg-2", "msg-1", "msg-1"}, "raw")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.AttemptID == "" {
		t.Fatal("prepared attempt ID is empty")
	}
	if got := strings.Join(prepared.MessageIDs, ","); got != "msg-1,msg-2" {
		t.Fatalf("message IDs = %q", got)
	}
	if err := writer.Result(prepared, OutcomeWritten, "terminal bytes accepted"); err != nil {
		t.Fatalf("Result: %v", err)
	}

	dir := filepath.Join(root, "agents", "codex", "receipts")
	for _, name := range []string{PreparedFilename, ResultFilename} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record Record
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &record); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if record.Agent != "codex" || record.AttemptID != prepared.AttemptID {
			t.Fatalf("%s record = %+v", name, record)
		}
	}
}

func TestListTreatsPreparedWithoutResultAsIndeterminate(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	prepared, err := writer.Prepare([]string{"msg-1"}, "external")
	if err != nil {
		t.Fatal(err)
	}

	attempts, err := List(root, "codex", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d", len(attempts))
	}
	if attempts[0].State != StateIndeterminate || attempts[0].Prepared.AttemptID != prepared.AttemptID {
		t.Fatalf("attempt = %+v", attempts[0])
	}
	if attempts[0].Result != nil {
		t.Fatalf("unexpected result: %+v", attempts[0].Result)
	}
}

func TestWriterRotatesOnceAndCapsBothGenerations(t *testing.T) {
	root := t.TempDir()
	writer := NewWriter(root, "codex")
	writer.maxBytes = 420

	for i := 0; i < 12; i++ {
		record := Record{
			Schema:     Schema,
			AttemptID:  strings.Repeat("a", 20) + string(rune('a'+i)),
			Phase:      PhasePrepared,
			MessageIDs: []string{"msg-" + string(rune('a'+i))},
			Agent:      "codex",
			Mode:       "raw",
			RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := writer.append(PreparedFilename, record); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	dir := filepath.Join(root, "agents", "codex", "receipts")
	for _, name := range []string{PreparedFilename, PreparedFilename + rotatedSuffix} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() > writer.maxBytes {
			t.Fatalf("%s size = %d, cap = %d", name, info.Size(), writer.maxBytes)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, PreparedFilename+rotatedSuffix+rotatedSuffix)); !os.IsNotExist(err) {
		t.Fatalf("unexpected second rotation: %v", err)
	}
}

func TestResultRejectsDishonestOutcome(t *testing.T) {
	writer := NewWriter(t.TempDir(), "codex")
	prepared, err := writer.Prepare([]string{"msg-1"}, "raw")
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"seen", "displayed", "submitted", ""} {
		if err := writer.Result(prepared, outcome, ""); err == nil {
			t.Fatalf("Result accepted outcome %q", outcome)
		}
	}
}
