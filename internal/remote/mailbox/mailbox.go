// Package mailbox is the amq-remote adapter kind "mailbox": a request is
// delivered as one AMQ message to a handle in serve's own root, and the
// handle's `amq reply` to that message is the request's result.
//
// The adapter keeps one journal per request under
// extensions/remote/mailbox/<target>/ in the root: <ref>.json fixes the
// message id, time and prompt before the first write, and records an
// outcome that delivers nothing or the acknowledgement; <ref>.outcome.json
// is the one decided terminal result, created exclusively; <ref>.lock
// serializes every call for the request and is never removed. A journal
// write counts only once the directory chain is synced.
package mailbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Kind is the manifest kind of this adapter.
const Kind = "mailbox"

// The carrier inboxes prompts come from, and so the inboxes the handle
// answers: buzz for a Buzz DM (as in amq-acp binding mode), amq-remote for
// every other origin.
const (
	buzzCarrier  = "buzz"
	localCarrier = "amq-remote"
)

const (
	defaultReplyTimeout = 10 * time.Minute
	maxReplyTimeout     = 24 * time.Hour
)

type config struct {
	Handle       string `json:"handle"`
	ReplyTimeout string `json:"reply_timeout,omitempty"`
}

// Factory builds a mailbox Attachment. It refuses unknown config fields, a
// handle that is a carrier inbox or the endpoint's own handle, an endpoint
// handle that is a carrier inbox, and a handle with no inbox in the root.
func Factory(_ context.Context, cfg registry.FactoryConfig) (core.Attachment, error) {
	var c config
	if len(cfg.Config) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Config))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("mailbox config: %w", err)
		}
	}
	if c.Handle == "" {
		return nil, errors.New("mailbox config: handle is required")
	}
	if err := fsq.ValidateHandle(c.Handle); err != nil {
		return nil, fmt.Errorf("mailbox config: invalid handle: %v", err)
	}
	switch {
	case isCarrier(c.Handle):
		return nil, fmt.Errorf("mailbox config: handle %q is a carrier inbox the adapter sends from", c.Handle)
	case c.Handle == cfg.Me:
		return nil, fmt.Errorf("mailbox config: handle %q is the endpoint's own handle", c.Handle)
	case isCarrier(cfg.Me):
		return nil, fmt.Errorf("mailbox: the endpoint handle %q is a carrier inbox; serve would import every reply as a command", cfg.Me)
	}
	// The target names the journal directory: a name that is not one path
	// segment on every platform is refused.
	if strings.Trim(cfg.Target, ".") == "" || strings.ContainsAny(cfg.Target, `:/\`) {
		return nil, fmt.Errorf("mailbox: target %q cannot name a journal directory", cfg.Target)
	}
	timeout := defaultReplyTimeout
	if c.ReplyTimeout != "" {
		d, err := time.ParseDuration(c.ReplyTimeout)
		if err != nil || d <= 0 || d > maxReplyTimeout {
			return nil, fmt.Errorf("mailbox config: reply_timeout must be a duration in (0, %s]", maxReplyTimeout)
		}
		timeout = d
	}
	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("mailbox: %w", err)
	}
	a := &Attachment{
		target: cfg.Target, handle: c.Handle, base: cfg.Root, identity: identity,
		replyTimeout: timeout, now: time.Now, inflight: map[string]int{},
	}
	root, err := a.open()
	if err != nil {
		return nil, fmt.Errorf("mailbox: %w", err)
	}
	defer func() { _ = root.Close() }()
	if !a.hasInbox(root) {
		return nil, fmt.Errorf("mailbox: handle %q has no inbox in %s; the adapter never creates one", c.Handle, cfg.Root)
	}
	return a, nil
}

func init() { registry.Register(Kind, Factory) }

func isCarrier(handle string) bool { return handle == buzzCarrier || handle == localCarrier }

// Attachment delivers requests to one AMQ handle. All durable state is the
// journal in the root; inflight only lets Forget refuse while a call for the
// same request is still running in this process.
type Attachment struct {
	target, handle string
	base           string
	identity       fsq.DeliveryRootIdentity
	replyTimeout   time.Duration
	now            func() time.Time

	mu       sync.Mutex
	inflight map[string]int
}

// entry is <ref>.json. A delivery entry carries the message id, time and
// prompt; Undelivered marks an entry for which nothing was or will be
// delivered (Cancelled when a cancel came first). Acked is set once the
// endpoint acknowledged the decided result.
type entry struct {
	MessageID   string             `json:"message_id,omitempty"`
	Created     string             `json:"created,omitempty"`
	Prompt      *acp.MailboxPrompt `json:"prompt,omitempty"`
	Undelivered string             `json:"undelivered,omitempty"`
	Cancelled   bool               `json:"cancelled,omitempty"`
	Acked       bool               `json:"acked,omitempty"`
}

// outcome is <ref>.outcome.json: the request's one terminal result and the
// replies the acknowledgement consumes.
type outcome struct {
	State     protocol.State     `json:"state"`
	Text      string             `json:"text"`
	Truncated bool               `json:"truncated,omitempty"`
	Replies   []acp.MailboxReply `json:"replies,omitempty"`
}

// NativeSessionID implements core.NativeIdentifier: the handle.
func (a *Attachment) NativeSessionID() string { return a.handle }

func (a *Attachment) epoch() string { return "mailbox:" + a.handle }

// Inspect implements core.Attachment. The target is live while the handle's
// inbox exists.
func (a *Attachment) Inspect() protocol.Session {
	attachment, status := "offline", "offline"
	if root, err := a.open(); err == nil {
		if a.hasInbox(root) {
			attachment, status = "live", "idle"
		}
		_ = root.Close()
	}
	return protocol.Session{
		Schema:      protocol.SchemaSession,
		TargetID:    a.target,
		Epoch:       a.epoch(),
		Harness:     Kind,
		DisplayName: "mailbox " + a.handle,
		Attachment:  attachment,
		Status:      status,
		Capabilities: protocol.Capabilities{
			Inspect: true, Submit: true, CancelRequest: true,
			Steer: false, ApproveTool: false, AnswerQuestion: false, Terminal: "unavailable",
		},
		Evidence:   &protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"},
		ObservedAt: protocol.FormatTime(a.now()),
	}
}

// Submit implements core.Attachment: it fixes the request's message in the
// journal, makes that durable, and only then delivers it. An entry that
// delivers nothing refuses without a write.
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	if req.Epoch != a.epoch() {
		return core.Admission{Code: protocol.CodeStaleEpoch, Message: "the mailbox target's epoch changed"}, nil
	}
	if req.NativeSession != "" && req.NativeSession != a.handle {
		return core.Admission{Code: protocol.CodeSessionChanged, Message: fmt.Sprintf("the target delivers to %s, not %s", a.handle, req.NativeSession)}, nil
	}
	ref := refOf(req.Key)
	defer a.enter(ref)()
	var adm core.Admission
	err := a.locked(ref, true, func(root *fsq.DeliveryRoot) error {
		var err error
		adm, err = a.submitLocked(root, ref, req)
		return err
	})
	return adm, err
}

func (a *Attachment) submitLocked(root *fsq.DeliveryRoot, ref string, req core.BoundRequest) (core.Admission, error) {
	ent, found, err := a.readEntry(root, ref)
	if err != nil {
		return core.Admission{}, err
	}
	if found && ent.Undelivered != "" {
		if ent.Cancelled {
			return core.Admission{Code: protocol.CodeCancelledBeforeAdmission, Message: ent.Undelivered}, nil
		}
		return core.Admission{Code: protocol.CodeNativeError, Message: notDelivered(a.handle, ent.Undelivered)}, nil
	}
	if found {
		_, decided, err := a.readOutcome(root, ref)
		if err != nil {
			return core.Admission{}, err
		}
		if decided || ent.Acked {
			return core.Admission{Admitted: true, RunID: ent.MessageID}, nil
		}
	}
	notAfter, err := protocol.ParseTime(req.NotAfter)
	if err != nil {
		return core.Admission{}, fmt.Errorf("mailbox: not_after: %w", err)
	}
	if !a.now().Before(notAfter) {
		return core.Admission{Code: protocol.CodeExpired, Message: "the submit window closed before delivery"}, nil
	}
	if !found {
		now := a.now()
		id, err := format.NewMessageID(now)
		if err != nil {
			return core.Admission{}, err
		}
		prompt := a.prompt(id, req)
		ent = entry{MessageID: id, Created: now.UTC().Format(time.RFC3339Nano), Prompt: &prompt}
		if err := a.createEntry(root, ref, ent); err != nil {
			return core.Admission{}, err
		}
	}
	// The entry may be visible without being durable (an earlier barrier
	// failed), so it is made durable before any message can exist.
	if err := a.barrier(root); err != nil {
		return core.Admission{}, err
	}
	created, err := time.Parse(time.RFC3339Nano, ent.Created)
	if err != nil || ent.Prompt == nil || ent.MessageID == "" {
		return core.Admission{}, fmt.Errorf("mailbox: journal entry %s is unreadable; refusing to deliver", ref)
	}
	err = acp.PublishMailboxPrompt(root, ent.MessageID, created, *ent.Prompt)
	if reason, refused := acp.NotDeliveredReason(err); refused {
		ent.Undelivered = reason
		if err := a.writeEntry(root, ref, ent); err != nil {
			return core.Admission{}, err
		}
		return core.Admission{Code: protocol.CodeNativeError, Message: notDelivered(a.handle, reason)}, nil
	}
	if err != nil {
		return core.Admission{}, err
	}
	return core.Admission{Admitted: true, RunID: ent.MessageID}, nil
}

// prompt is the message for req: a Buzz DM keeps amq-acp binding mode's
// sender, thread and labels; every other origin comes from amq-remote on
// one thread per creator host.
func (a *Attachment) prompt(id string, req core.BoundRequest) acp.MailboxPrompt {
	text := strings.TrimRight(req.Input.Text, "\n")
	o := req.Origin
	if o["carrier"] == "buzz" && o["event"] != "" && o["channel"] != "" {
		return acp.MailboxPrompt{
			To: a.handle, From: buzzCarrier, Thread: "cockpit/" + o["channel"], Subject: "Buzz DM",
			Labels: []string{"acp", "buzz", "nostr:" + o["event"]},
			Body: text + "\n\nThis came from the owner's Buzz DM. Answer with `amq reply --id " + id + "`; your reply is shown in that DM. " +
				"A reply with `--kind status` is not shown; any other reply is the final answer.",
		}
	}
	ref := refOf(req.Key)
	return acp.MailboxPrompt{
		To: a.handle, From: localCarrier, Thread: "remote/" + req.Key.CreatorHost, Subject: "remote request " + ref,
		Labels: []string{"remote", "request_ref:" + ref},
		Body:   text + "\n\nAnswer with `amq reply --id " + id + "`; your reply is the request's result.",
	}
}

// Lookup implements core.Attachment. It never delivers. A request with no
// entry gets a tombstone first, so a Submit that has not run yet delivers
// nothing; a decided result is returned unchanged on every call until it is
// acknowledged.
func (a *Attachment) Lookup(key requests.Key, _ string) (core.Evidence, error) {
	ref := refOf(key)
	defer a.enter(ref)()
	var ev core.Evidence
	err := a.locked(ref, false, func(root *fsq.DeliveryRoot) error {
		if root == nil {
			// A Submit, cancel or acknowledgement holds the request.
			ev = core.Evidence{Class: core.EvidenceTentative, Known: true}
			return nil
		}
		ent, found, err := a.readEntry(root, ref)
		if err != nil {
			return err
		}
		if !found {
			if err := a.createEntry(root, ref, entry{Undelivered: "its delivery never started"}); err != nil {
				return err
			}
		}
		if !found || ent.Undelivered != "" || ent.Acked {
			if err := a.barrier(root); err != nil {
				return err
			}
			ev = core.Evidence{Class: core.EvidenceNone}
			return nil
		}
		ev, err = a.observe(root, ref, ent)
		return err
	})
	return ev, err
}

// observe returns the evidence for a delivery entry: the decided result,
// else a result it decides now (a final reply, or the reply timeout), else
// running while the message is in the handle's inbox and unknown when it
// is not.
func (a *Attachment) observe(root *fsq.DeliveryRoot, ref string, ent entry) (core.Evidence, error) {
	out, decided, err := a.readOutcome(root, ref)
	if err != nil {
		return core.Evidence{}, err
	}
	if !decided {
		created, err := time.Parse(time.RFC3339Nano, ent.Created)
		if err != nil || ent.Prompt == nil {
			return core.Evidence{}, fmt.Errorf("mailbox: journal entry %s is unreadable", ref)
		}
		proposed, final, err := a.finalReply(root, ent, created)
		if err != nil {
			return core.Evidence{}, err
		}
		present, err := acp.InboxHasMessage(root, a.handle, ent.MessageID)
		if err != nil {
			return core.Evidence{}, err
		}
		switch {
		case final:
		case !a.now().Before(created.Add(a.replyTimeout)):
			proposed = outcome{State: protocol.StateFailed, Text: a.timeoutText(present)}
		case present:
			return core.Evidence{Class: core.EvidenceConfirmed, Known: true, Admitted: true, RunID: ent.MessageID, State: protocol.StateRunning}, nil
		default:
			// Absence does not prove non-delivery: the handle may have
			// read and removed the message.
			return core.Evidence{Class: core.EvidenceUnknown, Known: true, RunID: ent.MessageID}, nil
		}
		if out, err = a.decide(root, ref, proposed); err != nil {
			return core.Evidence{}, err
		}
	}
	if err := a.barrier(root); err != nil {
		return core.Evidence{}, err
	}
	return terminal(ent.MessageID, out), nil
}

// finalReply finds the newest reply to the prompt that is not a status and
// has a body. The proposed outcome consumes every matched reply up to it.
func (a *Attachment) finalReply(root *fsq.DeliveryRoot, ent entry, created time.Time) (outcome, bool, error) {
	replies, err := acp.FindReplies(root, ent.Prompt.From, a.handle, ent.Prompt.Thread, ent.MessageID, created)
	if err != nil {
		return outcome{}, false, err
	}
	for i := len(replies) - 1; i >= 0; i-- {
		r := replies[i]
		if r.Header.Kind == string(format.KindStatus) {
			continue
		}
		body, err := a.replyBody(root, ent.Prompt.From, r)
		if err != nil {
			return outcome{}, false, err
		}
		if body == "" {
			continue
		}
		text, truncated := protocol.TruncateText(body, protocol.MaxResultBytes)
		return outcome{State: protocol.StateCompleted, Text: text, Truncated: truncated, Replies: replies[:i+1]}, true, nil
	}
	return outcome{}, false, nil
}

// replyBody reads a matched reply from the box it was found in, or from cur
// when another consumer moved it since.
func (a *Attachment) replyBody(root *fsq.DeliveryRoot, inbox string, r acp.MailboxReply) (string, error) {
	var err error
	for _, box := range []string{r.Box, "cur"} {
		var msg format.Message
		msg, err = format.ReadMessageFileRoot(root, filepath.Join("agents", inbox, "inbox", box, r.Name))
		if err == nil {
			return strings.TrimSpace(msg.Body), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

func (a *Attachment) timeoutText(present bool) string {
	if present {
		return fmt.Sprintf("No final reply from %s yet. The message stays in its AMQ inbox and may still be answered.", a.handle)
	}
	return fmt.Sprintf("No final reply from %s. The message is not in its AMQ inbox: it was read and removed, or it never arrived.", a.handle)
}

func terminal(runID string, out outcome) core.Evidence {
	return core.Evidence{
		Class: core.EvidenceConfirmed, Known: true, Admitted: true, RunID: runID,
		State: out.State, Result: &protocol.Result{Text: out.Text, Truncated: out.Truncated},
	}
}

// CancelExact implements core.Attachment. Before delivery it records a
// cancel the late Submit obeys. After delivery mail cannot be unsent: the
// request ends as delivered and no longer waited for, unless a reply or the
// timeout decided it first.
func (a *Attachment) CancelExact(key requests.Key, _ string) (core.CancelEvidence, error) {
	ref := refOf(key)
	defer a.enter(ref)()
	var ev core.CancelEvidence
	err := a.locked(ref, true, func(root *fsq.DeliveryRoot) error {
		ent, found, err := a.readEntry(root, ref)
		if err != nil {
			return err
		}
		if !found {
			ent = entry{Undelivered: "cancelled before delivery", Cancelled: true}
			if err := a.createEntry(root, ref, ent); err != nil {
				return err
			}
		}
		var out outcome
		switch {
		case ent.Undelivered != "" || ent.Acked:
		default:
			present, err := acp.InboxHasMessage(root, a.handle, ent.MessageID)
			if err != nil {
				return err
			}
			if out, err = a.decide(root, ref, outcome{State: protocol.StateCancelled, Text: a.cancelText(present)}); err != nil {
				return err
			}
		}
		if err := a.barrier(root); err != nil {
			return err
		}
		ev = core.CancelEvidence{Disposition: protocol.CancelNoopTerminal}
		if ent.Cancelled || (ent.Undelivered == "" && !ent.Acked && out.State == protocol.StateCancelled) {
			ev.Disposition = protocol.CancelConfirmed
		}
		return nil
	})
	return ev, err
}

func (a *Attachment) cancelText(present bool) string {
	if present {
		return fmt.Sprintf("Delivered; stopped waiting. The message stays in %s's AMQ inbox.", a.handle)
	}
	return fmt.Sprintf("Stopped waiting. The message is not in %s's AMQ inbox: it was read and removed, or it never arrived.", a.handle)
}

// Respond implements core.Attachment: a mailbox has no interactions.
func (a *Attachment) Respond(requests.Key, string, string, string) (protocol.Code, error) {
	return protocol.CodeUnsupported, nil
}

// AcknowledgeResult implements core.Attachment.
func (a *Attachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	_ = a.AcknowledgeResultChecked(key, epoch, digest)
}

// AcknowledgeResultChecked implements core.CheckedAcknowledger. For the
// decided result with this digest it drains the replies it consumed for the
// carrier inbox, then marks the entry acknowledged; nil means both are
// durable. A reply that arrives after the decision is not consumed.
func (a *Attachment) AcknowledgeResultChecked(key requests.Key, _ string, digest string) error {
	ref := refOf(key)
	defer a.enter(ref)()
	return a.locked(ref, true, func(root *fsq.DeliveryRoot) error {
		ent, found, err := a.readEntry(root, ref)
		if err != nil || !found {
			return err
		}
		if !ent.Acked {
			out, decided, err := a.readOutcome(root, ref)
			if err != nil {
				return err
			}
			if !decided || ent.Prompt == nil || protocol.EvidenceDigest(terminal(ent.MessageID, out).Result) != digest {
				return fmt.Errorf("mailbox: request %s holds no result with digest %s", ref, digest)
			}
			for _, r := range out.Replies {
				if err := acp.ConsumeReply(root, ent.Prompt.From, r); err != nil {
					return err
				}
			}
			ent.Acked = true
			if err := a.writeEntry(root, ref, ent); err != nil {
				return err
			}
		}
		return a.barrier(root)
	})
}

// Forget implements core.Forgetter. It removes the journal of a request
// whose result was acknowledged or that delivered nothing, and refuses every
// other state and any call for the request still running in this process.
// The lock file stays: removing it would let two holders lock two inodes.
func (a *Attachment) Forget(key requests.Key) error {
	ref := refOf(key)
	// a.mu is held across the removal so no call for ref can enter before
	// it ends; a call that entered earlier makes Forget refuse.
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight[ref] > 0 {
		return fmt.Errorf("mailbox: request %s has a call in progress", ref)
	}
	return a.locked(ref, true, func(root *fsq.DeliveryRoot) error {
		ent, found, err := a.readEntry(root, ref)
		if err != nil {
			return err
		}
		if found && !ent.Acked && ent.Undelivered == "" {
			return fmt.Errorf("mailbox: request %s has a result the endpoint has not acknowledged", ref)
		}
		for _, name := range []string{ref + ".outcome.json", ref + ".json"} {
			if err := root.Remove(filepath.Join(a.dir(), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return a.barrier(root)
	})
}

// Subscribe implements core.Attachment. The adapter emits no events: the
// endpoint's reconcile looks up every open request on each tick.
func (a *Attachment) Subscribe(func(core.NativeEvent)) func() { return func() {} }

// enter counts a call for ref until the returned func runs.
func (a *Attachment) enter(ref string) func() {
	a.mu.Lock()
	a.inflight[ref]++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		if a.inflight[ref]--; a.inflight[ref] <= 0 {
			delete(a.inflight, ref)
		}
		a.mu.Unlock()
	}
}

// open opens the root capability with the identity seen at build and
// authenticates any inherited session pin on it.
func (a *Attachment) open() (*fsq.DeliveryRoot, error) {
	root, err := fsq.OpenDeliveryRoot(a.base, a.identity)
	if err != nil {
		return nil, err
	}
	if err := acp.VerifySessionPinOn(root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// locked runs fn on one root capability while it holds ref's lock. With
// wait false, a lock another call holds runs fn with a nil root instead.
func (a *Attachment) locked(ref string, wait bool, fn func(*fsq.DeliveryRoot) error) error {
	if !lock.AdvisoryLockAvailable() {
		return errors.New("mailbox: refusing to deliver without an advisory file lock")
	}
	root, err := a.open()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenLockFile(a.dir(), ref+".lock", 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if wait {
		return fsq.WithExclusiveFileLock(f, func() error { return fn(root) })
	}
	held, err := lock.TryLock(f)
	if err != nil {
		return err
	}
	if !held {
		return fn(nil)
	}
	defer func() { _ = lock.Unlock(f) }()
	return fn(root)
}

func (a *Attachment) dir() string { return filepath.Join("extensions", "remote", "mailbox", a.target) }

func (a *Attachment) hasInbox(root *fsq.DeliveryRoot) bool {
	info, err := root.Stat(filepath.Join("agents", a.handle, "inbox"))
	return err == nil && info.IsDir()
}

// barrier makes every journal write and removal so far durable: it syncs
// the journal directory and each ancestor up to the root. Until it returns
// nil the step has not happened.
func (a *Attachment) barrier(root *fsq.DeliveryRoot) error {
	return root.SyncDirChain(a.dir())
}

func (a *Attachment) readEntry(root *fsq.DeliveryRoot, ref string) (entry, bool, error) {
	var ent entry
	found, err := readJSON(root, filepath.Join(a.dir(), ref+".json"), &ent)
	return ent, found, err
}

func (a *Attachment) readOutcome(root *fsq.DeliveryRoot, ref string) (outcome, bool, error) {
	var out outcome
	found, err := readJSON(root, filepath.Join(a.dir(), ref+".outcome.json"), &out)
	if found && err == nil && !out.State.Terminal() {
		err = fmt.Errorf("mailbox: outcome of %s records no terminal state", ref)
	}
	return out, found, err
}

func readJSON(root *fsq.DeliveryRoot, name string, v any) (bool, error) {
	raw, err := root.ReadRegularNoFollow(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("mailbox: %s is unreadable: %w", name, err)
	}
	return true, nil
}

func (a *Attachment) createEntry(root *fsq.DeliveryRoot, ref string, ent entry) error {
	raw, err := json.Marshal(ent)
	if err != nil {
		return err
	}
	return root.CreateExclusiveFile(filepath.Join(a.dir(), ref+".json"), raw, 0o600)
}

func (a *Attachment) writeEntry(root *fsq.DeliveryRoot, ref string, ent entry) error {
	raw, err := json.Marshal(ent)
	if err != nil {
		return err
	}
	if _, err := root.WriteFileAtomic(a.dir(), ref+".json", raw, 0o600); err != nil {
		return err
	}
	return a.barrier(root)
}

// decide makes proposed the request's one terminal outcome by exclusive
// create, or returns the outcome already decided.
func (a *Attachment) decide(root *fsq.DeliveryRoot, ref string, proposed outcome) (outcome, error) {
	raw, err := json.Marshal(proposed)
	if err != nil {
		return outcome{}, err
	}
	err = root.CreateExclusiveFile(filepath.Join(a.dir(), ref+".outcome.json"), raw, 0o600)
	if err == nil {
		return proposed, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return outcome{}, err
	}
	out, found, err := a.readOutcome(root, ref)
	if err == nil && !found {
		err = fmt.Errorf("mailbox: outcome of %s vanished", ref)
	}
	return out, err
}

func notDelivered(handle, reason string) string {
	return fmt.Sprintf("Not delivered to %s: %s. Nothing reached its inbox; send it again.", handle, reason)
}

func refOf(key requests.Key) string {
	return protocol.EncodeRef(key.CreatorHost, key.TargetID, key.RequestID)
}
