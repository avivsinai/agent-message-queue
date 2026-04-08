package receipt

import (
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
	StageDrained = "drained"
	StageDLQ     = "dlq"
)

type Receipt struct {
	Schema    int    `json:"schema"`
	MsgID     string `json:"msg_id"`
	Thread    string `json:"thread,omitempty"`
	Sender    string `json:"sender"`
	Consumer  string `json:"consumer"`
	Stage     string `json:"stage"`
	EmittedAt string `json:"emitted_at"`
	Detail    string `json:"detail,omitempty"`
}

func New(msgID, thread, sender, consumer, stage, detail string) Receipt {
	return Receipt{
		Schema:    format.CurrentSchema,
		MsgID:     msgID,
		Thread:    thread,
		Sender:    sender,
		Consumer:  consumer,
		Stage:     stage,
		EmittedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Detail:    detail,
	}
}

func (r Receipt) filename() string {
	return fmt.Sprintf("%s__%s__%s.json", r.MsgID, r.Consumer, r.Stage)
}

func (r Receipt) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Emit writes a receipt to the consumer's receipts directory and
// best-effort mirrors it to the sender's receipts directory.
// The consumer-local write is canonical; mirroring never causes Emit to fail.
func Emit(root, consumer string, r Receipt) error {
	data, err := r.Marshal()
	if err != nil {
		return fmt.Errorf("receipt marshal: %w", err)
	}

	consumerDir := fsq.AgentReceipts(root, consumer)
	if _, err := fsq.WriteFileAtomic(consumerDir, r.filename(), data, 0o600); err != nil {
		return fmt.Errorf("receipt write (consumer %s): %w", consumer, err)
	}

	// Best-effort mirror to sender's namespace.
	if r.Sender != "" && r.Sender != consumer {
		senderDir := fsq.AgentReceipts(root, r.Sender)
		_, _ = fsq.WriteFileAtomic(senderDir, r.filename(), data, 0o600)
	}

	return nil
}

func Read(path string) (Receipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

type ListFilter struct {
	MsgID    string // filter by message ID
	Consumer string // filter by consumer
	Stage    string // filter by stage
}

func List(root, agent string, f ListFilter) ([]Receipt, error) {
	dir := fsq.AgentReceipts(root, agent)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var receipts []Receipt
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		// Pre-filter by filename components (msgID__consumer__stage.json)
		// to avoid reading and parsing files that can't match.
		if f.MsgID != "" && !strings.HasPrefix(name, f.MsgID+"__") {
			continue
		}
		if f.Stage != "" && !strings.HasSuffix(name, "__"+f.Stage+".json") {
			continue
		}
		if f.Consumer != "" && !strings.Contains(name, "__"+f.Consumer+"__") {
			continue
		}
		r, err := Read(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		receipts = append(receipts, r)
	}

	sort.Slice(receipts, func(i, j int) bool {
		return receipts[i].EmittedAt < receipts[j].EmittedAt
	})

	return receipts, nil
}
