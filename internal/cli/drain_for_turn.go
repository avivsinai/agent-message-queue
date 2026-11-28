package cli

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

// passivePreviewLimit is the most Unicode code points a digest preview holds,
// its truncation mark included.
const passivePreviewLimit = 160

// forTurnRemaining counts the unread messages a --for-turn drain leaves, by
// tier. Eligible counts wake-eligible messages that are not urgent.
type forTurnRemaining struct {
	Urgent   int `json:"urgent"`
	Eligible int `json:"eligible"`
	Passive  int `json:"passive"`
}

// drainForTurnResult is the --for-turn JSON. Drained holds the urgent and
// other wake-eligible messages with full bodies, and any message moved to
// DLQ; Passive holds one digest per passive message. Count is every message
// the command claimed: len(Drained) + len(Passive).
type drainForTurnResult struct {
	Drained   []drainItem      `json:"drained"`
	Passive   []passiveDigest  `json:"passive"`
	Count     int              `json:"count"`
	Remaining forTurnRemaining `json:"remaining"`
}

// passiveDigest is the compact record of one claimed passive message. Read
// is the command that prints the complete original from inbox/cur.
type passiveDigest struct {
	ID        string   `json:"id"`
	From      string   `json:"from"`
	Thread    string   `json:"thread"`
	Kind      string   `json:"kind,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	Subject   string   `json:"subject"`
	Refs      []string `json:"refs,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Preview   string   `json:"preview"`
	Truncated bool     `json:"truncated"`
	Read      string   `json:"read"`
}

// forTurnTier ranks a message for a turn: urgent first, then other
// wake-eligible mail, then passive mail.
func forTurnTier(item inboxItem) int {
	switch {
	case item.Priority == format.PriorityUrgent:
		return 0
	case item.Passive:
		return 2
	default:
		return 1
	}
}

// orderForTurn orders items by tier, each tier in drain order, and takes the
// first limit of them (0 = all).
func orderForTurn(items []inboxItem, limit int) []inboxItem {
	items = slices.Clone(items)
	format.SortByTimestamp(items)
	slices.SortStableFunc(items, func(a, b inboxItem) int { return cmp.Compare(forTurnTier(a), forTurnTier(b)) })
	if limit > 0 && limit < len(items) {
		items = items[:limit]
	}
	return items
}

// countForTurn counts items by tier.
func countForTurn(items []inboxItem) forTurnRemaining {
	var n forTurnRemaining
	for _, item := range items {
		switch forTurnTier(item) {
		case 0:
			n.Urgent++
		case 1:
			n.Eligible++
		default:
			n.Passive++
		}
	}
	return n
}

// digestPassive is the compact record of a claimed passive message. The
// preview is the body with each run of white space made one space.
func digestPassive(me string, item inboxItem) passiveDigest {
	preview := []rune(strings.Join(strings.Fields(item.Body), " "))
	truncated := len(preview) > passivePreviewLimit
	if truncated {
		preview = append(preview[:passivePreviewLimit-1], '…')
	}
	return passiveDigest{
		ID:        item.ID,
		From:      item.From,
		Thread:    item.Thread,
		Kind:      item.Kind,
		Priority:  item.Priority,
		Subject:   item.Subject,
		Refs:      item.Refs,
		Labels:    item.Labels,
		Preview:   string(preview),
		Truncated: truncated,
		Read:      "amq read --me " + shellQuoteArg(me) + " --id " + shellQuoteArg(item.ID),
	}
}

func drainForTurnItems(deliveryRoot *fsq.DeliveryRoot, root, me string, limit int, validator *headerValidator) ([]inboxItem, forTurnRemaining, error) {
	var items []inboxItem
	var rest forTurnRemaining
	err := deliveryRoot.WithPinnedBatch(func(batch *fsq.DeliveryRoot) error {
		var err error
		items, rest, err = drainForTurnPinned(batch, root, me, limit, validator)
		return err
	})
	return items, rest, err
}

// drainForTurnPinned is drainInboxItemsPinned with the claim set chosen by
// orderForTurn. The order comes from a read of inbox/new; the output comes
// from the claimed copies only. A file that cannot be read there stays
// wake-eligible, and its claim reports it as drain always does.
func drainForTurnPinned(deliveryRoot *fsq.DeliveryRoot, root, me string, limit int, validator *headerValidator) ([]inboxItem, forTurnRemaining, error) {
	promoteFollowUps(deliveryRoot, me)
	repairErr := receipt.RepairPending(deliveryRoot, me)
	filenames, err := collectInboxFilenames(deliveryRoot, me)
	if err != nil {
		return nil, forTurnRemaining{}, joinOutcomeErr(err, repairErr)
	}
	if validator == nil {
		validator = &headerValidator{}
	}
	unread := make([]inboxItem, 0, len(filenames))
	for _, filename := range filenames {
		item, err := readInboxItem(deliveryRoot, filepath.Join("agents", me, "inbox", "new", filename), filename, false, validator)
		if os.IsNotExist(err) {
			continue
		}
		unread = append(unread, item)
	}
	picked := orderForTurn(unread, limit)
	filenames = filenames[:0]
	for _, item := range picked {
		filenames = append(filenames, item.Filename)
	}
	items, err := claimInboxFilesPinned(deliveryRoot, root, me, filenames, true, 0, validator, nil)
	// A batch that stops early leaves picked files unread: count every file
	// this drain did not claim.
	claimed := make(map[string]bool, len(items))
	for _, item := range items {
		claimed[item.Filename] = true
	}
	unread = slices.DeleteFunc(unread, func(item inboxItem) bool { return claimed[item.Filename] })
	return orderForTurn(items, 0), countForTurn(unread), joinOutcomeErr(err, repairErr)
}

// outputForTurn prints wake-eligible mail in full, then one digest per
// passive message, then the counts still unread.
func outputForTurn(jsonOutput bool, me string, items []inboxItem, rest forTurnRemaining) error {
	full := make([]drainItem, 0, len(items))
	digests := make([]passiveDigest, 0)
	for _, item := range items {
		if item.Passive {
			digests = append(digests, digestPassive(me, item))
		} else {
			full = append(full, item)
		}
	}
	if jsonOutput {
		return writeJSON(os.Stdout, drainForTurnResult{Drained: full, Passive: digests, Count: len(items), Remaining: rest})
	}
	if err := writeStdout("[AMQ] %d new message(s) for %s:\n\n", len(items), me); err != nil {
		return err
	}
	for _, item := range full {
		if err := writeDrainItemText(item, true); err != nil {
			return err
		}
	}
	if len(digests) > 0 {
		if err := writeStdout("[AMQ] %d passive message(s), as a digest:\n\n", len(digests)); err != nil {
			return err
		}
	}
	for _, d := range digests {
		if err := writePassiveDigestText(d); err != nil {
			return err
		}
	}
	next := ""
	switch {
	case rest.Urgent+rest.Eligible > 0:
		next = " Run amq drain --for-turn again for the actionable mail."
	case rest.Passive > 0:
		next = " Passive mail needs no turn of its own."
	}
	return writeStdout("[AMQ] Unread: %d urgent, %d other wake-eligible, %d passive.%s\n", rest.Urgent, rest.Eligible, rest.Passive, next)
}

func writePassiveDigestText(d passiveDigest) error {
	subject := d.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	priority := d.Priority
	if priority == "" {
		priority = "-"
	}
	kind := d.Kind
	if kind == "" {
		kind = "-"
	}
	if err := writeStdout("- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n",
		d.From, d.Thread, d.ID, subject, priority, kind); err != nil {
		return err
	}
	for _, list := range []struct {
		label  string
		values []string
	}{{"Refs", d.Refs}, {"Labels", d.Labels}} {
		if len(list.values) == 0 {
			continue
		}
		if err := writeStdout("  %s: %s\n", list.label, strings.Join(list.values, ", ")); err != nil {
			return err
		}
	}
	return writeStdout("  Preview: %s\n  Read: %s\n---\n", d.Preview, d.Read)
}
