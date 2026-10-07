package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// CockpitPromptSubject labels prompts delivered on the durable cockpit thread.
const CockpitPromptSubject = "ACP cockpit prompt"

// CockpitSteeringSubject labels steering delivered on the cockpit thread.
const CockpitSteeringSubject = "ACP steering"

// Delivery is the durable outcome of one prompt turn. The message is queued in
// the recipient's inbox; nothing here proves the recipient consumed it.
type Delivery struct {
	MessageID string
	To        string
	Thread    string
	Created   time.Time
	EventID   string
	State     string
	Committed bool
	Drained   bool
	Started   bool
	Completed bool
	Egress    string
	Duplicate bool
}

// DeliverCockpitPrompt sends a prompt on the stable cockpit thread used by the
// live ACP bridge. The returned creation time is the lower bound for reply
// polling, so an older message on the same thread cannot answer this turn.
func DeliverCockpitPrompt(cfg Config, body, thread, eventID string) (Delivery, error) {
	return deliver(cfg, body, thread, CockpitPromptSubject, format.PriorityNormal, []string{"acp", "cockpit"}, nil, eventID)
}

// DeliverSteering sends steering on the session's cockpit thread. During an
// in-flight turn (turnPrompt set) it is urgent with the buzz-steer label,
// AMQ's native interrupt signal, and refs the turn's prompt, so a peer's
// reply to the steer still answers that turn; while idle it is an ordinary
// normal-priority message.
func DeliverSteering(cfg Config, body, thread, turnPrompt, eventID string) (Delivery, error) {
	if turnPrompt == "" {
		return deliver(cfg, body, thread, CockpitSteeringSubject, format.PriorityNormal, []string{"acp"}, nil, eventID)
	}
	return deliver(cfg, body, thread, CockpitSteeringSubject, format.PriorityUrgent, []string{"acp", "buzz-steer"}, []string{turnPrompt}, eventID)
}

func deliver(cfg Config, body, thread, subject, priority string, labels, refs []string, eventID string) (Delivery, error) {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Delivery{}, fmt.Errorf("prompt contains no text content")
	}
	if strings.TrimSpace(thread) == "" {
		return Delivery{}, fmt.Errorf("message thread is empty")
	}

	// One canonical builder appends the nostr label exactly once (review of
	// #976 P1 a): the first attempt and a replay must serialize identical
	// bytes, or the no-replace inbox write refuses one of them as a
	// collision.
	build := func(messageID, created string) ([]byte, error) {
		all := append(append([]string(nil), labels...), "nostr:"+eventID)
		data, err := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       messageID,
				From:     cfg.Me,
				To:       []string{cfg.To},
				Thread:   thread,
				Subject:  subject,
				Created:  created,
				Priority: priority,
				Labels:   all,
				Refs:     refs,
			},
			Body: body,
		}.Marshal()
		if err != nil {
			return nil, err
		}
		if len(data) > format.MaxMessageSize {
			return nil, fmt.Errorf("prompt exceeds the maximum AMQ message size")
		}
		return data, nil
	}

	// With an event id the whole claim-check-write sequence runs under one
	// cross-process per-event lock (review of #976 P1 b): claim
	// create/load, the inbox absence check and the inbox write are one
	// critical section, so a paused winner cannot write a second copy into
	// new/ after a replay's copy was drained to cur/, and two processes
	// cannot both pass the absence check. The lock shape is the mailbox
	// path's per-event post lock (publishClaimed); the journal lives under
	// the outbox acp-events dir.
	if eventID != "" {
		if !lock.AdvisoryLockAvailable() {
			return Delivery{}, fmt.Errorf("refusing to deliver a Buzz event without an advisory file lock")
		}
		identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
		if err != nil {
			return Delivery{}, err
		}
		root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
		if err != nil {
			return Delivery{}, err
		}
		defer func() { _ = root.Close() }()
		dir := filepath.Dir(eventJournalPath(cfg.Me, eventID))
		lockFile, err := root.OpenLockFile(dir, eventID+".post.lock", 0o600)
		if err != nil {
			return Delivery{}, fmt.Errorf("open event post lock: %w", err)
		}
		defer func() { _ = lockFile.Close() }()
		var out Delivery
		err = fsq.WithExclusiveFileLock(lockFile, func() error {
			out, err = deliverLocked(cfg, root, build, thread, eventID)
			return err
		})
		return out, err
	}

	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return Delivery{}, err
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		return Delivery{}, err
	}
	defer func() { _ = root.Close() }()
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return Delivery{}, err
	}
	data, err := build(id, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Delivery{}, err
	}
	return writeToInbox(cfg, root, claimIdentity("", id, cfg.To, thread), now, data)
}

// deliverLocked serves one event-keyed delivery attempt while holding the
// event's post lock. Ordering rule (review of #976 r3): under the lock, a
// claim that exists is proven against the inbox FIRST — a delivered event
// reports Duplicate/Committed even if its journal is not yet durable,
// because the delivery already happened and must never be reported as an
// error — and the claim's durability is required ONLY before an ABSENT
// message is written (fresh create and adopt alike): fsync of the claim
// file itself through the pinned root, then the full directory chain
// acp-events -> outbox -> agent dir, so the journal survives a crash before
// anything names it.
func deliverLocked(cfg Config, root *fsq.DeliveryRoot, build func(messageID, created string) ([]byte, error), thread, eventID string) (Delivery, error) {
	claim, durable, err := claimEvent(cfg, root, build, thread, eventID)
	if err != nil {
		return Delivery{}, err
	}
	created := time.Time{}
	if claim.Created != "" {
		parsed, err := time.Parse(time.RFC3339Nano, claim.Created)
		if err != nil {
			return Delivery{}, fmt.Errorf("event %s has an unreadable claim time", eventID)
		}
		created = parsed
	}
	delivered, err := inboxHasMessage(root, claim.To, claim.MessageID)
	if err != nil {
		return Delivery{}, err
	}
	if delivered {
		// A legacy journal (no created) proves its time from the delivered
		// message header before reporting the duplicate (review of #976 P2 d).
		if created.IsZero() {
			created, err = provenCreated(root, claim.To, claim.MessageID)
			if err != nil {
				return Delivery{}, err
			}
		}
		return Delivery{
			MessageID: claim.MessageID,
			To:        claim.To,
			Thread:    claim.Thread,
			Created:   created,
			EventID:   claim.EventID,
			State:     DeliveryDuplicate,
			Committed: true,
			Egress:    EgressConfirmed,
			Duplicate: true,
		}, nil
	}
	// The message is absent: the claim must be durable before the inbox
	// write names it. A failure refuses the attempt; a replay retries.
	if err := durable(); err != nil {
		return Delivery{}, err
	}

	// A legacy journal with no created time and no delivered message takes
	// its header time from the delivered message too (review of #976 P2 d).
	if claim.Created == "" {
		data, err := build(claim.MessageID, "")
		if err != nil {
			return Delivery{}, err
		}
		msg, err := format.ParseMessage(data)
		if err != nil {
			return Delivery{}, err
		}
		claim.Created = msg.Header.Created
		created, _ = time.Parse(time.RFC3339Nano, claim.Created)
		data, err = build(claim.MessageID, claim.Created)
		if err != nil {
			return Delivery{}, err
		}
		return writeToInbox(cfg, root, &claim, created, data)
	}

	data, err := build(claim.MessageID, claim.Created)
	if err != nil {
		return Delivery{}, err
	}
	return writeToInbox(cfg, root, &claim, created, data)
}

// claimEvent loads the event's claim or creates it exclusively. It returns
// a durability callback the caller runs ONLY before writing an absent inbox
// message (review of #976 r3): on EVERY path — fresh create and adopt
// alike — it fsyncs the claim file itself through the pinned root
// (CreateExclusiveFile writes the final name directly; a creator killed
// between write and sync leaves unsynced content) and the full directory
// chain acp-events -> outbox -> agent dir (a freshly created acp-events
// needs its parent synced). A claim that cannot be made durable refuses
// the attempt; a replay retries the sync.
func claimEvent(cfg Config, root *fsq.DeliveryRoot, build func(messageID, created string) ([]byte, error), thread, eventID string) (eventRecord, func() error, error) {
	if rec, err := readEventClaim(cfg, root, eventID); err != nil {
		return eventRecord{}, nil, err
	} else if rec != nil {
		return *rec, func() error { return syncEventClaim(root, cfg.Me, rec.EventID) }, nil
	}
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return eventRecord{}, nil, err
	}
	rec := eventRecord{
		Schema:    format.CurrentSchema,
		EventID:   eventID,
		MessageID: id,
		To:        cfg.To,
		Thread:    thread,
		Created:   now.UTC().Format(time.RFC3339Nano),
	}
	if err := writeEventClaimExclusive(root, cfg.Me, rec); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return eventRecord{}, nil, err
		}
		// A concurrent winner claimed first under the same post lock (a
		// legacy process may not hold it); follow its claim.
		winner, err := readEventClaim(cfg, root, eventID)
		if err != nil {
			return eventRecord{}, nil, err
		}
		if winner == nil {
			return eventRecord{}, nil, fmt.Errorf("event %s lost its claim and the claim is unreadable", eventID)
		}
		return *winner, func() error { return syncEventClaim(root, cfg.Me, winner.EventID) }, nil
	}
	return rec, func() error { return syncEventClaim(root, cfg.Me, rec.EventID) }, nil
}

// syncEventClaim makes an existing claim durable before an absent message
// is delivered under it: the claim file's own content is fsynced through
// the pinned root (a creator killed after its write but before its sync
// leaves unsynced content a replay must repair), then the journal
// directory chain — acp-events, its parent outbox, and the agent dir — so
// a freshly created journal directory survives too. A failure here leaves
// the attempt refused: the next attempt retries the sync, and nothing is
// published until one succeeds.
func syncEventClaim(root *fsq.DeliveryRoot, me, eventID string) error {
	journalPath := eventJournalPath(me, eventID)
	file, _, err := root.OpenRegularNoFollow(journalPath)
	if err != nil {
		return fmt.Errorf("reopen the event claim for sync: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr == nil {
		syncErr = fsq.FileSyncFaultForTest()
	}
	if syncErr != nil {
		return fmt.Errorf("sync the event claim file: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close the event claim after sync: %w", closeErr)
	}
	if err := root.SyncDir(filepath.Dir(journalPath)); err != nil {
		return fmt.Errorf("sync the event claim directory: %w", err)
	}
	if err := root.SyncDir(filepath.Join("agents", me, "outbox")); err != nil {
		return fmt.Errorf("sync the outbox directory: %w", err)
	}
	if err := root.SyncDir(filepath.Join("agents", me)); err != nil {
		return fmt.Errorf("sync the agent directory: %w", err)
	}
	return nil
}

// readEventClaim reads the claim of an event, if one exists, through the
// already-open delivery root.
func readEventClaim(cfg Config, root *fsq.DeliveryRoot, eventID string) (*eventRecord, error) {
	data, err := root.ReadRegularNoFollow(eventJournalPath(cfg.Me, eventID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec eventRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.MessageID == "" || rec.To == "" || rec.Thread == "" || fsq.ValidateHandle(rec.To) != nil {
		return nil, fmt.Errorf("event %s has an unreadable claim; refusing to deliver", eventID)
	}
	if rec.Created != "" {
		if _, err := time.Parse(time.RFC3339Nano, rec.Created); err != nil {
			return nil, fmt.Errorf("event %s has an unreadable claim time", eventID)
		}
	}
	return &rec, nil
}

// createExclusiveFile publishes raw at relPath only if nothing is there
// yet: CreateExclusiveFile creates the name with O_EXCL and fsyncs the
// content before the name is visible, then the journal directory is fsynced
// (review of #976 P1 c). The parent chain (outbox, agent dir) is synced by
// the adopt barrier (syncEventClaim) before any inbox write names the
// claim, which also covers a freshly created acp-events directory.
func createExclusiveFile(root *fsq.DeliveryRoot, relPath string, raw []byte) error {
	if err := root.CreateExclusiveFile(relPath, raw, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return root.SyncDir(filepath.Dir(relPath))
}

// writeEventClaimExclusive durably creates the event's claim journal.
func writeEventClaimExclusive(root *fsq.DeliveryRoot, me string, rec eventRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return createExclusiveFile(root, eventJournalPath(me, rec.EventID), append(data, '\n'))
}

// provenCreated reads the delivered message's header Created time from the
// recipient inbox. A legacy schema-1 journal records no created field; its
// replay proves the time from the message it delivered (review of #976
// P2 d).
func provenCreated(root *fsq.DeliveryRoot, handle, messageID string) (time.Time, error) {
	name := messageID + ".md"
	for _, rel := range []string{
		filepath.Join("agents", handle, "inbox", "new", name),
		filepath.Join("agents", handle, "inbox", "cur", name),
	} {
		msg, err := format.ReadMessageFileRoot(root, rel)
		if err == nil {
			return time.Parse(time.RFC3339Nano, msg.Header.Created)
		}
		if !os.IsNotExist(err) {
			return time.Time{}, err
		}
	}
	return time.Time{}, fmt.Errorf("message %s is in %s's inbox but its header is unreadable", messageID, handle)
}

// claimIdentity builds the in-memory identity of a message being delivered
// first-hand, shaped like a claim so writeToInbox serves both paths.
func claimIdentity(eventID, messageID, to, thread string) *eventRecord {
	return &eventRecord{EventID: eventID, MessageID: messageID, To: to, Thread: thread}
}

// inboxHasMessage reports whether the recipient's inbox (new or cur) already
// holds the claimed message: the only delivery proof a replay trusts. Both
// locations are inspected through the pinned delivery root with a no-follow
// regular-file read (review of #976 r2 P2): a directory or a symlink named
// <id>.md is not a delivery, and the entry must parse as the claimed message
// (header id equals the claimed id) — anything else refuses instead of
// reporting a delivery that is not one.
func inboxHasMessage(root *fsq.DeliveryRoot, handle, messageID string) (bool, error) {
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
// inbox and reports what the write proves; a CommittedDurabilityError stays
// uncertain egress, on the first attempt and on a replay alike.
func writeToInbox(cfg Config, root *fsq.DeliveryRoot, claim *eventRecord, created time.Time, data []byte) (Delivery, error) {
	if claim == nil {
		return Delivery{}, fmt.Errorf("writeToInbox requires a claim")
	}
	id, thread, to, eventID := claim.MessageID, claim.Thread, claim.To, claim.EventID
	if to == "" {
		to = cfg.To
	}
	_, err := fsq.DeliverToInboxes(root, []string{to}, id+".md", data)
	egress := EgressConfirmed
	if err != nil {
		var uncertain *fsq.CommittedDurabilityError
		if !errors.As(err, &uncertain) {
			return Delivery{}, err
		}
		egress = EgressUncertain
	}
	return Delivery{
		MessageID: id,
		To:        to,
		Thread:    thread,
		Created:   created,
		EventID:   eventID,
		State:     DeliveryStateQueued,
		Committed: true,
		Egress:    egress,
	}, nil
}
