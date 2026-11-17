package mailprompt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Prompt is one prompt for an AMQ handle: the handle it goes to, the
// inbox the handle answers (From), and the exact thread, subject, labels and
// body. Publish adds nothing to it.
type Prompt struct {
	To      string   `json:"to"`
	From    string   `json:"from"`
	Thread  string   `json:"thread"`
	Subject string   `json:"subject"`
	Labels  []string `json:"labels,omitempty"`
	// Context is the header's context object, written unchanged.
	Context map[string]any `json:"context,omitempty"`
	Body    string         `json:"body"`
}

// Publish delivers p to p.To as message id unless that message
// is already in its inbox (new or cur), so a redelivery with the same id,
// time and prompt never duplicates it. It never creates the handle's mailbox
// or inbox. Once the message is known absent, a refusal by the handle's
// mailbox is an error NotDeliveredReason recognizes; any other error leaves
// the outcome unknown. A committed write whose sync failed is delivered.
func Publish(root *fsq.DeliveryRoot, id string, created time.Time, p Prompt) error {
	// The handle answers p.From with `amq reply`, which cannot create a
	// missing mailbox in a root without config.json, so the sender ensures
	// its own. This runs before the redelivery check: a prompt delivered
	// earlier still needs the mailbox for its reply.
	if err := root.EnsureAgentDirs(p.From); err != nil {
		return err
	}
	// A configured root must durably know the carrier as a reply recipient
	// before this publisher delivers a prompt.
	if _, err := config.EnsureAgentIfConfiguredOn(root, p.From); err != nil {
		return err
	}
	// Only a readable message with this id, read through the pinned root,
	// proves the earlier delivery: a directory or a dangling link at the
	// name is not one.
	if held, err := InboxHasMessage(root, p.To, id); err != nil {
		return err
	} else if held {
		return nil
	}
	message := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     p.From,
			To:       []string{p.To},
			Thread:   p.Thread,
			Subject:  p.Subject,
			Created:  created.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityNormal,
			Labels:   p.Labels,
			Context:  p.Context,
		},
		Body: p.Body,
	}
	data, err := message.Marshal()
	if err != nil {
		return err
	}
	if len(data) > format.MaxMessageSize {
		return fmt.Errorf("prompt exceeds the maximum AMQ message size")
	}
	if err := completeMailbox(root, p.To); err != nil {
		return err
	}
	if _, err := fsq.DeliverToExistingInbox(root, p.To, id+".md", data); err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return &notDeliveredError{reason: "its inbox refused the message: " + err.Error()}
		}
	}
	return nil
}

// NotDeliveredReason returns the reason when err from Publish
// proves that nothing reached the handle's inbox.
func NotDeliveredReason(err error) (string, bool) {
	var refused *notDeliveredError
	if errors.As(err, &refused) {
		return refused.reason, true
	}
	return "", false
}

// completeMailbox refuses delivery unless the handle's mailbox and inbox
// already exist as real directories (not symlinks) under the pinned root,
// and creates the other leaves an older amq did not make, such as
// receipts/. Every open, create, and sync goes through capabilities derived
// from root, so a root or agents/ replaced meanwhile refuses delivery.
func completeMailbox(root *fsq.DeliveryRoot, handle string) error {
	agents, err := root.OpenDirectChild("agents")
	if err != nil {
		return anchorRefusal(err, "no mailbox in its AMQ root")
	}
	defer func() { _ = agents.Close() }()
	mailbox, err := agents.OpenDirectChild(handle)
	if err != nil {
		return anchorRefusal(err, "no mailbox in its AMQ root")
	}
	defer func() { _ = mailbox.Close() }()
	inbox, err := mailbox.OpenDirectChild("inbox")
	if err != nil {
		return anchorRefusal(err, "its mailbox has no inbox")
	}
	defer func() { _ = inbox.Close() }()
	if err := repairMailbox(mailbox, inbox); err != nil {
		return &notDeliveredError{reason: err.Error()}
	}
	return nil
}

// anchorRefusal is the refusal for a mailbox anchor that could not be
// opened: missing when it is absent from an unchanged root.
func anchorRefusal(err error, missing string) error {
	if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fsq.ErrDeliveryRootChanged) {
		return &notDeliveredError{reason: missing}
	}
	return &notDeliveredError{reason: err.Error()}
}

// repairMailbox creates the missing leaves of the handle mailbox, opened as
// mailbox and its inbox as inbox. It never creates either one: leaves are
// made through the opened directories, so one removed meanwhile makes this
// fail instead of reappearing.
func repairMailbox(mailbox, inbox *fsq.DeliveryRoot) error {
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		dir, path := mailbox, string(leaf)
		if rest, ok := strings.CutPrefix(path, "inbox/"); ok {
			dir, path = inbox, rest
		}
		if err := ensureLeaf(dir, strings.Split(path, "/")); err != nil {
			return fmt.Errorf("complete its mailbox: %w", err)
		}
	}
	return nil
}

// ensureLeaf opens each directory of names below parent in turn, creating a
// missing one and syncing its parent so the new entry is durable before a
// message lands.
func ensureLeaf(parent *fsq.DeliveryRoot, names []string) error {
	if len(names) == 0 {
		return nil
	}
	child, err := parent.OpenDirectChild(names[0])
	if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fsq.ErrDeliveryRootChanged) {
		if child, err = parent.OpenOrCreateDirectChild(names[0], 0o700); err == nil {
			err = parent.SyncDir(".")
		}
	}
	if err != nil {
		if child != nil {
			_ = child.Close()
		}
		return err
	}
	defer func() { _ = child.Close() }()
	return ensureLeaf(child, names[1:])
}

// notDeliveredError means Publish found the message absent from the
// handle's inbox and the inbox then refused it: nothing was delivered.
type notDeliveredError struct{ reason string }

func (e *notDeliveredError) Error() string { return e.reason }

// InboxHasMessage reports whether the recipient's inbox (new or cur) already
// holds the claimed message: the only delivery proof a replay trusts. Both
// locations are inspected through the pinned delivery root with a no-follow
// regular-file read: a directory or a symlink named <id>.md is not a
// delivery, and the entry must parse as the claimed message
// (header id equals the claimed id) — anything else refuses instead of
// reporting a delivery that is not one.
func InboxHasMessage(root *fsq.DeliveryRoot, handle, messageID string) (bool, error) {
	name := messageID + ".md"
	for _, rel := range []string{
		filepath.Join("agents", handle, "inbox", "new", name),
		filepath.Join("agents", handle, "inbox", "cur", name),
	} {
		msg, err := format.ReadMessageFileRoot(root, rel)
		if err == nil {
			if msg.Header.ID != messageID {
				return false, fmt.Errorf("inbox holds %s under the claimed name but its header id is %s", rel, msg.Header.ID)
			}
			return true, nil
		}
		if os.IsNotExist(err) {
			continue
		}
		return false, fmt.Errorf("the claimed inbox entry %s is not a readable message: %w", rel, err)
	}
	return false, nil
}

// writeToInbox delivers an already-serialized message into the recipient
