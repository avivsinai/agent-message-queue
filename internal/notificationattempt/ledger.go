package notificationattempt

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	Schema = "amq/notification-attempt/v1"

	PhasePrepared = "prepared"
	PhaseResult   = "result"

	OutcomeWritten = "written"
	OutcomeFailed  = "failed"

	StateIndeterminate = "indeterminate"

	PreparedFilename = "notification-attempts.prepared.jsonl"
	ResultFilename   = "notification-attempts.result.jsonl"
	rotatedSuffix    = ".1"

	defaultMaxBytes = 256 * 1024
)

type Record struct {
	Schema     string   `json:"schema"`
	AttemptID  string   `json:"attempt_id"`
	Phase      string   `json:"phase"`
	MessageIDs []string `json:"message_ids"`
	Agent      string   `json:"agent"`
	Mode       string   `json:"mode"`
	RecordedAt string   `json:"recorded_at"`
	Outcome    string   `json:"outcome,omitempty"`
	Detail     string   `json:"detail,omitempty"`
}

type Attempt struct {
	State    string  `json:"state"`
	Prepared Record  `json:"prepared"`
	Result   *Record `json:"result,omitempty"`
}

type Writer struct {
	root     string
	agent    string
	maxBytes int64
	now      func() time.Time
}

func NewWriter(root, agent string) *Writer {
	return &Writer{
		root:     root,
		agent:    agent,
		maxBytes: defaultMaxBytes,
		now:      time.Now,
	}
}

func (w *Writer) Prepare(messageIDs []string, mode string) (Record, error) {
	if err := fsq.ValidateHandle(w.agent); err != nil {
		return Record{}, fmt.Errorf("notification attempt agent: %w", err)
	}
	ids := normalizedMessageIDs(messageIDs)
	if len(ids) == 0 {
		return Record{}, fmt.Errorf("notification attempt requires at least one message id")
	}
	attemptID, err := format.NewMessageID(w.now())
	if err != nil {
		return Record{}, fmt.Errorf("notification attempt id: %w", err)
	}
	record := Record{
		Schema:     Schema,
		AttemptID:  attemptID,
		Phase:      PhasePrepared,
		MessageIDs: ids,
		Agent:      w.agent,
		Mode:       strings.TrimSpace(mode),
		RecordedAt: w.now().UTC().Format(time.RFC3339Nano),
	}
	if err := w.append(PreparedFilename, record); err != nil {
		return Record{}, fmt.Errorf("persist prepared notification attempt: %w", err)
	}
	return record, nil
}

func (w *Writer) Result(prepared Record, outcome, detail string) error {
	if outcome != OutcomeWritten && outcome != OutcomeFailed {
		return fmt.Errorf("notification attempt outcome must be %q or %q", OutcomeWritten, OutcomeFailed)
	}
	if prepared.Phase != PhasePrepared || prepared.AttemptID == "" ||
		prepared.Agent != w.agent || len(prepared.MessageIDs) == 0 {
		return fmt.Errorf("invalid prepared notification attempt")
	}
	record := Record{
		Schema:     Schema,
		AttemptID:  prepared.AttemptID,
		Phase:      PhaseResult,
		MessageIDs: append([]string{}, prepared.MessageIDs...),
		Agent:      w.agent,
		Mode:       prepared.Mode,
		RecordedAt: w.now().UTC().Format(time.RFC3339Nano),
		Outcome:    outcome,
		Detail:     strings.TrimSpace(detail),
	}
	if err := w.append(ResultFilename, record); err != nil {
		return fmt.Errorf("persist notification attempt result: %w", err)
	}
	return nil
}

func (w *Writer) append(filename string, record Record) error {
	if w.maxBytes <= 0 {
		return fmt.Errorf("notification attempt journal size cap must be positive")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if int64(len(data)) > w.maxBytes {
		return fmt.Errorf("notification attempt record is %d bytes, cap is %d", len(data), w.maxBytes)
	}

	identity, err := fsq.SnapshotDeliveryRoot(w.root)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(w.root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	dir := filepath.Join("agents", w.agent, "receipts")
	path := filepath.Join(dir, filename)
	current, err := root.ReadRegularNoFollow(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		current = nil
	}
	if int64(len(current)+len(data)) > w.maxBytes {
		if len(current) > 0 {
			if _, err := root.WriteFileAtomic(dir, filename+rotatedSuffix, current, 0o600); err != nil {
				return fmt.Errorf("rotate notification attempt journal: %w", err)
			}
		}
		current = nil
	}
	next := make([]byte, 0, len(current)+len(data))
	next = append(next, current...)
	next = append(next, data...)
	if _, err := root.WriteFileAtomic(dir, filename, next, 0o600); err != nil {
		return err
	}
	return nil
}

func List(root, agent, messageID string) ([]Attempt, error) {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return nil, err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = deliveryRoot.Close() }()
	return ListDeliveryRoot(deliveryRoot, agent, messageID)
}

func ListDeliveryRoot(root *fsq.DeliveryRoot, agent, messageID string) ([]Attempt, error) {
	if err := fsq.ValidateHandle(agent); err != nil {
		return nil, fmt.Errorf("notification attempt agent: %w", err)
	}
	prepared, err := readRecords(root, agent, PreparedFilename, PhasePrepared)
	if err != nil {
		return nil, err
	}
	results, err := readRecords(root, agent, ResultFilename, PhaseResult)
	if err != nil {
		return nil, err
	}
	resultByAttempt := make(map[string]Record, len(results))
	for _, result := range results {
		resultByAttempt[result.AttemptID] = result
	}
	preparedByAttempt := make(map[string]Record, len(prepared))
	for _, item := range prepared {
		preparedByAttempt[item.AttemptID] = item
	}
	var attempts []Attempt
	for _, item := range preparedByAttempt {
		if messageID != "" && !contains(item.MessageIDs, messageID) {
			continue
		}
		attempt := Attempt{State: StateIndeterminate, Prepared: item}
		if result, ok := resultByAttempt[item.AttemptID]; ok {
			resultCopy := result
			attempt.State = result.Outcome
			attempt.Result = &resultCopy
		}
		attempts = append(attempts, attempt)
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		return attempts[i].Prepared.RecordedAt < attempts[j].Prepared.RecordedAt
	})
	return attempts, nil
}

func readRecords(root *fsq.DeliveryRoot, agent, filename, phase string) ([]Record, error) {
	dir := filepath.Join("agents", agent, "receipts")
	var records []Record
	for _, name := range []string{filename + rotatedSuffix, filename} {
		path := filepath.Join(dir, name)
		data, err := root.ReadRegularNoFollow(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read notification attempt journal %s: %w", path, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 4096), defaultMaxBytes)
		line := 0
		for scanner.Scan() {
			line++
			if strings.TrimSpace(scanner.Text()) == "" {
				continue
			}
			var record Record
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				return nil, fmt.Errorf("parse notification attempt journal %s line %d: %w", path, line, err)
			}
			if err := validateRecord(record, agent, phase); err != nil {
				return nil, fmt.Errorf("parse notification attempt journal %s line %d: %w", path, line, err)
			}
			records = append(records, record)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("scan notification attempt journal %s: %w", path, err)
		}
	}
	return records, nil
}

func validateRecord(record Record, agent, phase string) error {
	if record.Schema != Schema || record.Agent != agent || record.Phase != phase ||
		record.AttemptID == "" || len(record.MessageIDs) == 0 || record.RecordedAt == "" {
		return fmt.Errorf("invalid %s record", phase)
	}
	if phase == PhasePrepared && record.Outcome != "" {
		return fmt.Errorf("prepared record must not claim an outcome")
	}
	if phase == PhaseResult && record.Outcome != OutcomeWritten && record.Outcome != OutcomeFailed {
		return fmt.Errorf("result outcome must be %q or %q", OutcomeWritten, OutcomeFailed)
	}
	return nil
}

func normalizedMessageIDs(messageIDs []string) []string {
	seen := make(map[string]bool, len(messageIDs))
	ids := make([]string, 0, len(messageIDs))
	for _, raw := range messageIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
