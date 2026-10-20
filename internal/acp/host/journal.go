package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Turn states. A record moves forward only: claimed, then dispatching, then
// answered, then replied (or undeliverable when no retry can deliver the
// reply); or claimed, then skipped.
const (
	stateClaimed     = "claimed"
	stateDispatching = "dispatching"
	stateAnswered    = "answered"
	stateReplied     = "replied"
	stateSkipped     = "skipped"
	// stateUndeliverable is terminal: the reply cannot form a deliverable
	// message, and Reason says why.
	stateUndeliverable = "undeliverable"
)

// turnRecord is the journal of one inbound message. The reply id and its
// creation time are fixed when the message is claimed, so a reply delivered
// again after a crash has the same bytes and the inbox keeps one copy.
type turnRecord struct {
	State        string `json:"state"`
	MessageID    string `json:"message_id"`
	File         string `json:"file"`
	From         string `json:"from"`
	Thread       string `json:"thread"`
	Subject      string `json:"subject,omitempty"`
	Kind         string `json:"kind,omitempty"`
	ReplyID      string `json:"reply_id"`
	ReplyCreated string `json:"reply_created"`
	Created      string `json:"created"`
	RequestID    int64  `json:"request_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	ReplyBody    string `json:"reply_body,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func turnsDir(me string) string {
	return filepath.Join("agents", me, "extensions", extensionLayer, "turns")
}

// claimTurn creates the record exclusively and syncs it before the message
// is moved. os.ErrExist means a record for this message already exists.
func claimTurn(root *fsq.DeliveryRoot, me string, rec turnRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir := turnsDir(me)
	if err := root.CreateExclusiveFile(filepath.Join(dir, rec.MessageID+".json"), append(data, '\n'), 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return root.SyncDir(dir)
}

// saveTurn replaces the record atomically and durably.
func saveTurn(root *fsq.DeliveryRoot, me string, rec turnRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = root.WriteFileAtomic(turnsDir(me), rec.MessageID+".json", append(data, '\n'), 0o600)
	return err
}

// loadTurn reads one record; a missing record is (nil, nil).
func loadTurn(root *fsq.DeliveryRoot, me, messageID string) (*turnRecord, error) {
	data, err := root.ReadFile(filepath.Join(turnsDir(me), messageID+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rec turnRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("turn record %s is unreadable: %w", messageID, err)
	}
	if rec.MessageID != messageID || rec.State == "" {
		return nil, fmt.Errorf("turn record %s is unreadable", messageID)
	}
	return &rec, nil
}

// listTurns names every record in the journal.
func listTurns(root *fsq.DeliveryRoot, me string) ([]string, error) {
	entries, err := root.ReadDir(turnsDir(me))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && filepath.Ext(name) == ".json" && name[0] != '.' {
			ids = append(ids, name[:len(name)-len(".json")])
		}
	}
	return ids, nil
}
