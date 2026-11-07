package receipt

import (
	"encoding/json"
	"errors"
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

// Emit writes a receipt to the consumer's receipts directory.
// Receipts live only in the consumer namespace; callers that need to observe
// them must read from the delivery root where the consumer emitted them.
func Emit(root string, r Receipt) error {
	data, err := r.Marshal()
	if err != nil {
		return fmt.Errorf("receipt marshal: %w", err)
	}

	consumerDir := fsq.AgentReceipts(root, r.Consumer)
	if _, err := fsq.WriteFileAtomicFollowLinks(consumerDir, r.filename(), data, 0o600); err != nil {
		return fmt.Errorf("receipt write (consumer %s): %w", r.Consumer, err)
	}

	return nil
}

// EmitDeliveryRoot writes a receipt through an already authorized root.
func EmitDeliveryRoot(root *fsq.DeliveryRoot, r Receipt) error {
	data, err := r.Marshal()
	if err != nil {
		return fmt.Errorf("receipt marshal: %w", err)
	}
	dir := filepath.Join("agents", r.Consumer, "receipts")
	if _, err := root.WriteFileAtomic(dir, r.filename(), data, 0o600); err != nil {
		return fmt.Errorf("receipt write (consumer %s): %w", r.Consumer, err)
	}
	return nil
}

// pendingDir holds receipts a consumer owes but could not write. It is a
// sibling of receipts/ so a failing receipts directory cannot also lose the
// obligation. Each entry is the full receipt under its final filename; an
// invalid entry is renamed with a quarantineSuffix, which the scan skips.
//
// The obligation is recorded only after the receipt write fails, not ahead of
// the claim, so a claim costs no extra fsync. A crash between the claim and
// the receipt write, or a failed park (for example a full disk), therefore
// still leaves that receipt unwritten.
func pendingDir(consumer string) string {
	return filepath.Join("agents", consumer, "receipts-pending")
}

const quarantineSuffix = ".bad"

// RepairError reports receipts that RepairPending could not write or entries
// it quarantined. The owed receipts stay parked for the next repair.
type RepairError struct{ Err error }

func (e *RepairError) Error() string { return "receipt repair: " + e.Err.Error() }
func (e *RepairError) Unwrap() error { return e.Err }

// EmitOwed writes a receipt for an outcome that already happened. When the
// write fails, the receipt is parked in the consumer's pending directory so
// RepairPending can write it later, and the write error is returned either
// way: the caller's outcome is committed but its receipt is not yet visible.
func EmitOwed(root *fsq.DeliveryRoot, r Receipt) error {
	writeErr := EmitDeliveryRoot(root, r)
	if writeErr == nil {
		return nil
	}
	data, err := r.Marshal()
	if err == nil {
		_, err = root.WriteFileAtomic(pendingDir(r.Consumer), r.filename(), data, 0o600)
	}
	if err != nil {
		return errors.Join(writeErr, fmt.Errorf("park %s receipt for %s: %w", r.Stage, r.MsgID, err))
	}
	return fmt.Errorf("%w (parked; the next drain or read writes it)", writeErr)
}

// RepairPending writes every receipt parked for consumer and then removes its
// pending entry. It only reads the pending directory, so its cost follows
// owed receipts, not mailbox history. A repeated or concurrent repair writes
// identical bytes and is harmless.
func RepairPending(root *fsq.DeliveryRoot, consumer string) error {
	dir := pendingDir(consumer)
	entries, err := root.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read pending receipts: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := root.ReadRegularNoFollow(path)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("read pending receipt %s: %w", name, err))
			}
			continue
		}
		r, err := parseReceipt(data)
		if err == nil && (r.Consumer != consumer || r.filename() != name) {
			err = fmt.Errorf("receipt names %s", r.filename())
		}
		if err != nil {
			// An invalid entry can never become a receipt. Move it aside once
			// so it is reported once instead of failing every later repair.
			bad := fmt.Sprintf("%s.%d%s", name, time.Now().UnixNano(), quarantineSuffix)
			if mvErr := root.RenameNoReplace(path, filepath.Join(dir, bad)); mvErr != nil && !os.IsNotExist(mvErr) {
				err = errors.Join(err, fmt.Errorf("quarantine: %w", mvErr))
			} else if mvErr == nil {
				err = fmt.Errorf("%w (moved to %s)", err, bad)
			}
			errs = append(errs, fmt.Errorf("invalid pending receipt %s: %w", name, err))
			continue
		}
		if err := EmitDeliveryRoot(root, r); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := root.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove pending receipt %s: %w", name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &RepairError{Err: errors.Join(errs...)}
}

// WaitFor polls for a specific consumer-local receipt by deterministic filename.
// Returns the receipt on match, or an error on timeout.
func WaitFor(root, msgID, consumer, stage string, timeout, pollInterval time.Duration) (Receipt, error) {
	name := fmt.Sprintf("%s__%s__%s.json", msgID, consumer, stage)
	path := filepath.Join(fsq.AgentReceipts(root, consumer), name)

	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for {
		r, err := Read(path)
		if err == nil {
			return r, nil
		}
		if !os.IsNotExist(err) {
			return Receipt{}, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return Receipt{}, os.ErrDeadlineExceeded
		}
		time.Sleep(pollInterval)
	}
}

// WaitForDeliveryRoot polls through the same capability used for delivery so a
// renamed root cannot redirect receipt observation to another tree.
func WaitForDeliveryRoot(root *fsq.DeliveryRoot, msgID, consumer, stage string, timeout, pollInterval time.Duration) (Receipt, error) {
	name := fmt.Sprintf("%s__%s__%s.json", msgID, consumer, stage)
	path := filepath.Join("agents", consumer, "receipts", name)
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		data, err := root.ReadRegularNoFollow(path)
		if err == nil {
			return parseReceipt(data)
		}
		if !os.IsNotExist(err) {
			return Receipt{}, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return Receipt{}, os.ErrDeadlineExceeded
		}
		time.Sleep(pollInterval)
	}
}

func Read(path string) (Receipt, error) {
	data, err := fsq.ReadRegularNoFollow(path)
	if err != nil {
		return Receipt{}, err
	}
	return parseReceipt(data)
}

// ReadDeliveryRoot reads a receipt through an already authorized queue root.
func ReadDeliveryRoot(root *fsq.DeliveryRoot, path string) (Receipt, error) {
	data, err := root.ReadRegularNoFollow(path)
	if err != nil {
		return Receipt{}, err
	}
	return parseReceipt(data)
}

func parseReceipt(data []byte) (Receipt, error) {
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
