package host

// The host plays the extension side of the bridge file seam
// (docs/pi-bridge-protocol.md) under its own protocol string, so amq-remote
// attaches to it as kind "acp" and the owner drives the same ACP session
// from Buzz. The files live beside the host's own state:
//
//	<root>/agents/<me>/extensions/acp-host/
//	  requests/<ref>.json    adapter -> host: one request per ref
//	  receipts/<ref>.json    host -> adapter: admission proof
//	  events/<ref>.jsonl     host -> adapter: lifecycle, approvals, outcome
//	  answers/<hash>.json    adapter -> host: one approval answer
//	  cancels/<ref>.json     adapter -> host: cancel of one request
//	  bridge.liveness        host -> adapter: heartbeat
//
// The host writes only receipts/, events/ and bridge.liveness. A request
// runs only between turns and only while the inbox holds nothing; otherwise
// it is refused busy. Its text goes to the agent verbatim.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// BridgeProtocol is the protocol string of every receipt, event line,
// liveness record and answer on the host's bridge seam.
const BridgeProtocol = "amq:acp-host:v1"

const (
	// bridgeRevision is the rule set the host implements: revision 3 plus
	// tool approvals through answers/ (revision 4) and exact cancel through
	// cancels/ (revision 5).
	bridgeRevision = 5
	// minRequestRevision is the lowest revision a request may require.
	minRequestRevision = 3
	livenessInterval   = 2 * time.Second
	// approvalTTL is how long an approval waits for the owner before the
	// host's policy (reject) answers it.
	approvalTTL  = 5 * time.Minute
	answerPoll   = 250 * time.Millisecond
	maxEventText = 256 * 1024
	maxPrompt    = 2000
)

var refPattern = regexp.MustCompile(`^amqr1_[a-z2-7]{16,472}$`)

// sessionIDPattern is the native session id grammar the adapter accepts.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$`)

type bridgeRequest struct {
	Ref            string `json:"ref"`
	Text           string `json:"text"`
	DeliverAs      string `json:"deliver_as"`
	NotAfter       string `json:"not_after"`
	EpochHint      string `json:"epoch_hint"`
	BridgeRevision int    `json:"bridge_revision"`
}

type bridgeReceipt struct {
	Protocol          string `json:"protocol"`
	Ref               string `json:"ref"`
	SessionGeneration string `json:"session_generation"`
	DeliveredAt       string `json:"delivered_at"`
	PID               int    `json:"pid"`
}

type bridgeEvent struct {
	Protocol      string   `json:"protocol"`
	Ref           string   `json:"ref"`
	Event         string   `json:"event"`
	Text          string   `json:"text,omitempty"`
	Error         string   `json:"error,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	At            string   `json:"at"`
	InteractionID string   `json:"interaction_id,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Prompt        string   `json:"prompt,omitempty"`
	Options       []string `json:"options,omitempty"`
	ApproveOption string   `json:"approve_option,omitempty"`
	RejectOption  string   `json:"reject_option,omitempty"`
	ManifestHash  string   `json:"manifest_hash,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
	Presence      string   `json:"presence,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	Option        string   `json:"option,omitempty"`
}

type bridgeLiveness struct {
	Protocol          string `json:"protocol"`
	Live              bool   `json:"live"`
	At                string `json:"at"`
	PID               int    `json:"pid"`
	Surface           string `json:"surface"`
	SessionGeneration string `json:"session_generation"`
	BridgeRevision    int    `json:"bridge_revision"`
	SessionID         string `json:"session_id,omitempty"`
	Agent             string `json:"agent,omitempty"`
}

type bridgeCancel struct {
	Protocol string `json:"protocol"`
	Ref      string `json:"ref"`
}

type bridgeAnswer struct {
	Protocol      string `json:"protocol"`
	Ref           string `json:"ref"`
	InteractionID string `json:"interaction_id"`
	ManifestHash  string `json:"manifest_hash"`
	Option        string `json:"option"`
}

// bridgeTurn is the remote request a turn serves.
type bridgeTurn struct {
	ref string
	// next numbers the turn's approvals; ended is set once the terminal
	// event is written, after which no approval line may follow. Both are
	// guarded by Host.eventMu.
	next  int
	ended bool
}

// errClaimed means another process already holds the request's receipt.
var errClaimed = errors.New("the request's receipt already exists")

func bridgePath(me string, parts ...string) string {
	return filepath.Join(append([]string{"agents", me, "extensions", extensionLayer}, parts...)...)
}

func isTerminal(event string) bool {
	switch event {
	case "completed", "failed", "cancelled", "refused", "uncertain":
		return true
	}
	return false
}

// startBridge mints the session generation, closes the requests an earlier
// host left running, publishes liveness, and keeps it fresh until stop.
func (h *Host) startBridge() (stop func(), err error) {
	var gen [12]byte
	if _, err := rand.Read(gen[:]); err != nil {
		return nil, err
	}
	h.generation = hex.EncodeToString(gen[:])
	h.handled = map[string]bool{}
	if err := h.withRoot(func(root *fsq.DeliveryRoot) error {
		if err := h.recoverBridge(root); err != nil {
			return err
		}
		return h.writeLiveness(root, true)
	}); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(livenessInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				_ = h.withRoot(func(root *fsq.DeliveryRoot) error { return h.writeLiveness(root, false) })
				return
			case <-ticker.C:
				_ = h.withRoot(func(root *fsq.DeliveryRoot) error { return h.writeLiveness(root, true) })
			}
		}
	}()
	return func() { close(done); wg.Wait() }, nil
}

func (h *Host) writeLiveness(root *fsq.DeliveryRoot, live bool) error {
	rec := bridgeLiveness{
		Protocol:          BridgeProtocol,
		Live:              live,
		At:                time.Now().UTC().Format(time.RFC3339Nano),
		PID:               os.Getpid(),
		Surface:           "acp",
		SessionGeneration: h.generation,
		BridgeRevision:    bridgeRevision,
		Agent:             h.agentName,
	}
	if sessionIDPattern.MatchString(h.sessionID) {
		rec.SessionID = h.sessionID
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = root.WriteFileAtomic(bridgePath(h.cfg.Me), "bridge.liveness", append(data, '\n'), 0o600)
	return err
}

// recoverBridge appends uncertain to every receipt an earlier host left
// without a terminal event: its turn may have run, and nothing proves how
// it ended. Every request with a receipt or a terminal event is handled.
func (h *Host) recoverBridge(root *fsq.DeliveryRoot) error {
	for _, ref := range h.listRefs(root, "receipts", ".json") {
		h.handled[ref] = true
		events, err := h.readEvents(root, ref)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(events, func(ev bridgeEvent) bool { return isTerminal(ev.Event) }) {
			if err := h.appendEvent(root, ref, nil, bridgeEvent{Event: "uncertain", Error: "the host restarted during this request; it did not run again"}); err != nil {
				return err
			}
		}
	}
	for _, ref := range h.listRefs(root, "events", ".jsonl") {
		events, err := h.readEvents(root, ref)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(events, func(ev bridgeEvent) bool { return isTerminal(ev.Event) }) {
			h.handled[ref] = true
		}
	}
	return nil
}

// listRefs names the refs of one bridge directory, in name order.
func (h *Host) listRefs(root *fsq.DeliveryRoot, dir, ext string) []string {
	entries, err := root.ReadDir(bridgePath(h.cfg.Me, dir))
	if err != nil {
		return nil
	}
	var refs []string
	for _, e := range entries {
		if ref, ok := strings.CutSuffix(e.Name(), ext); ok && e.Type().IsRegular() && refPattern.MatchString(ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

func (h *Host) readEvents(root *fsq.DeliveryRoot, ref string) ([]bridgeEvent, error) {
	data, err := root.ReadFile(bridgePath(h.cfg.Me, "events", ref+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []bridgeEvent
	for _, line := range strings.Split(string(data), "\n") {
		var ev bridgeEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Ref == ref {
			out = append(out, ev)
		}
	}
	return out, nil
}

// appendEvent appends one fsynced line to the ref's event stream. A trailing
// fragment from a failed write is closed with a newline first, so it stays
// one skipped line. bt, when set, is the turn that owns the ref: no approval
// line follows its terminal event.
func (h *Host) appendEvent(root *fsq.DeliveryRoot, ref string, bt *bridgeTurn, ev bridgeEvent) error {
	h.eventMu.Lock()
	defer h.eventMu.Unlock()
	if bt != nil {
		if bt.ended && !isTerminal(ev.Event) {
			return nil
		}
		if isTerminal(ev.Event) {
			bt.ended = true
		}
	}
	ev.Protocol, ev.Ref, ev.At = BridgeProtocol, ref, time.Now().UTC().Format(time.RFC3339Nano)
	ev.Text, ev.Error = cutUTF8(ev.Text, maxEventText), cutUTF8(ev.Error, maxEventText)
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	dir, name := bridgePath(h.cfg.Me, "events"), ref+".jsonl"
	if prior, err := root.ReadFile(filepath.Join(dir, name)); err == nil && len(prior) > 0 && prior[len(prior)-1] != '\n' {
		line = append([]byte{'\n'}, line...)
	}
	_, err = root.AppendLedgerLine(dir, name, line)
	return err
}

func cutUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// pendingRequests names the requests not yet handled, oldest publication
// first.
func (h *Host) pendingRequests(root *fsq.DeliveryRoot) []string {
	type entry struct {
		ref string
		mod time.Time
	}
	var pending []entry
	for _, ref := range h.listRefs(root, "requests", ".json") {
		if h.handled[ref] {
			continue
		}
		info, err := root.Stat(bridgePath(h.cfg.Me, "requests", ref+".json"))
		if err != nil {
			continue
		}
		pending = append(pending, entry{ref, info.ModTime()})
	}
	slices.SortFunc(pending, func(a, b entry) int {
		if c := a.mod.Compare(b.mod); c != 0 {
			return c
		}
		return strings.Compare(a.ref, b.ref)
	})
	refs := make([]string, len(pending))
	for i, p := range pending {
		refs[i] = p.ref
	}
	return refs
}

// admit applies the shape, revision, deadline and generation rules to one
// request. A request that fails one is refused and handled; ok is true for a
// request that may run once the host is idle.
func (h *Host) admit(root *fsq.DeliveryRoot, ref string) (bridgeRequest, bool, error) {
	if _, err := root.Stat(bridgePath(h.cfg.Me, "receipts", ref+".json")); err == nil {
		h.handled[ref] = true
		return bridgeRequest{}, false, nil
	}
	refuse := func(reason, detail string) (bridgeRequest, bool, error) {
		h.handled[ref] = true
		return bridgeRequest{}, false, h.appendEvent(root, ref, nil, bridgeEvent{Event: "refused", Reason: reason, Error: detail})
	}
	if h.cancelRequested(root, ref) {
		return refuse("cancelled", "the owner cancelled the request before it ran")
	}
	data, err := root.ReadFile(bridgePath(h.cfg.Me, "requests", ref+".json"))
	if err != nil {
		return bridgeRequest{}, false, nil
	}
	var req bridgeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return refuse("invalid", "the request does not parse")
	}
	notAfter, err := time.Parse(time.RFC3339Nano, req.NotAfter)
	switch {
	case req.Ref != ref:
		return refuse("invalid", "the request names another ref")
	case strings.TrimSpace(req.Text) == "":
		return refuse("invalid", "the request has no text")
	case req.DeliverAs != "followUp":
		return refuse("unsupported", "deliver_as must be followUp")
	case err != nil:
		return refuse("invalid", "not_after does not parse")
	case req.BridgeRevision < minRequestRevision || req.BridgeRevision > bridgeRevision:
		return refuse("revision", fmt.Sprintf("this host implements bridge revisions %d to %d", minRequestRevision, bridgeRevision))
	case !time.Now().Before(notAfter):
		return refuse("expired", "")
	case req.EpochHint != h.generation:
		return refuse("generation", "the request addresses another session generation")
	}
	return req, true, nil
}

// cancelRequested reports whether the adapter published a cancel for ref.
func (h *Host) cancelRequested(root *fsq.DeliveryRoot, ref string) bool {
	data, err := root.ReadFile(bridgePath(h.cfg.Me, "cancels", ref+".json"))
	if err != nil {
		return false
	}
	var c bridgeCancel
	return json.Unmarshal(data, &c) == nil && c.Protocol == BridgeProtocol && c.Ref == ref
}

// inboxBusy reports whether a peer message waits in the inbox. A file the
// host leaves in place does not count.
func (h *Host) inboxBusy(root *fsq.DeliveryRoot) bool {
	entries, err := root.ReadDir(filepath.Join("agents", h.cfg.Me, "inbox", "new"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(name, ".md") && !strings.HasPrefix(name, ".") && !h.unreadable[name] {
			return true
		}
	}
	return false
}

// refuseBusy refuses every admissible request busy: a turn is in flight.
func (h *Host) refuseBusy(root *fsq.DeliveryRoot) {
	for _, ref := range h.pendingRequests(root) {
		if _, ok, err := h.admit(root, ref); err == nil && ok {
			h.handled[ref] = true
			_ = h.appendEvent(root, ref, nil, bridgeEvent{Event: "refused", Reason: "busy", Error: "the agent is running another turn"})
		}
	}
}

// serveRequests runs the oldest admissible request while the inbox is
// empty, and refuses the others busy. The error ends the host: an event
// that could not be written, or an agent that exited.
func (h *Host) serveRequests(ctx context.Context, root *fsq.DeliveryRoot) error {
	for _, ref := range h.pendingRequests(root) {
		if ctx.Err() != nil {
			return nil
		}
		// A request refused busy while an earlier one ran is decided.
		if h.handled[ref] {
			continue
		}
		req, ok, err := h.admit(root, ref)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if h.inboxBusy(root) {
			h.handled[ref] = true
			if err := h.appendEvent(root, ref, nil, bridgeEvent{Event: "refused", Reason: "busy", Error: "peer messages are waiting"}); err != nil {
				return err
			}
			continue
		}
		if err := h.runRequest(ctx, root, req); err != nil {
			return err
		}
	}
	return nil
}

// runRequest runs one request as one turn. The receipt is written create-new
// and the started event appended immediately before the prompt bytes.
func (h *Host) runRequest(ctx context.Context, root *fsq.DeliveryRoot, req bridgeRequest) error {
	bt := &bridgeTurn{ref: req.Ref}
	res, err := h.prompt(ctx, root, req.Text, nil, bt, func(int64) error {
		data, err := json.Marshal(bridgeReceipt{
			Protocol:          BridgeProtocol,
			Ref:               req.Ref,
			SessionGeneration: h.generation,
			DeliveredAt:       time.Now().UTC().Format(time.RFC3339Nano),
			PID:               os.Getpid(),
		})
		if err != nil {
			return err
		}
		h.handled[req.Ref] = true
		dir := bridgePath(h.cfg.Me, "receipts")
		if err := root.CreateExclusiveFile(filepath.Join(dir, req.Ref+".json"), append(data, '\n'), 0o600); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errClaimed
			}
			return err
		}
		if err := root.SyncDir(dir); err != nil {
			return err
		}
		return h.appendEvent(root, req.Ref, bt, bridgeEvent{Event: "started"})
	})
	if errors.Is(err, errClaimed) {
		return nil
	}
	if err != nil {
		return err
	}
	ev, fatal := requestOutcome(res, h.cfg.AgentExit)
	if err := h.appendEvent(root, req.Ref, bt, ev); err != nil {
		return err
	}
	return fatal
}

// requestOutcome maps a turn's end to the request's terminal event, and
// whether the host must stop afterwards.
func requestOutcome(res turnResult, exitStatus func() string) (bridgeEvent, error) {
	if res.err != nil {
		var rerr *rpcError
		switch {
		case errors.As(res.err, &rerr):
			return bridgeEvent{Event: "failed", Error: "the agent failed this turn: " + rerr.Message}, nil
		case errors.Is(res.err, errClosed):
			status := "status unknown"
			if exitStatus != nil {
				status = exitStatus()
			}
			return bridgeEvent{Event: "failed", Error: "the agent exited during this turn (" + status + "); its effects are unknown"}, res.err
		default:
			return bridgeEvent{Event: "failed", Error: "the turn failed: " + res.err.Error() + "; its effects are unknown"}, res.err
		}
	}
	var out struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res.raw, &out)
	switch out.StopReason {
	case "end_turn":
		return bridgeEvent{Event: "completed", Text: res.answer}, nil
	case "cancelled":
		return bridgeEvent{Event: "cancelled"}, nil
	default:
		return bridgeEvent{Event: "failed", Error: joinNote(res.answer, "The turn stopped: "+stopLabel(out.StopReason)+".")}, nil
	}
}

// askOwner shows one tool approval of a remote request's turn on the remote
// face and waits for the owner's answer until it expires. The host offers
// the remote face only the reject_once option (presence local_only), and the
// host's own policy, which also rejects, answers at expiry. It returns false
// when the turn is cancelled or ends meanwhile: the approval then answers
// cancelled.
func (h *Host) askOwner(turn *turnCollect, req permissionRequest, reject int) bool {
	bt := turn.bridge
	h.eventMu.Lock()
	bt.next++
	id := fmt.Sprintf("tool-%d", bt.next)
	h.eventMu.Unlock()

	names := make([]string, len(req.Options))
	approve := ""
	for i, option := range req.Options {
		names[i] = option.Name
		if names[i] == "" {
			names[i] = option.OptionID
		}
		if approve == "" && option.Kind == "allow_once" {
			approve = names[i]
		}
	}
	rejectName := names[reject]
	sum := sha256.Sum256(req.ToolCall)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	expires := time.Now().Add(approvalTTL)
	line := bridgeEvent{
		Event:         "interaction",
		InteractionID: id,
		Kind:          "approval",
		Prompt:        cutUTF8(approvalPrompt(req.ToolCall), maxPrompt),
		Options:       names,
		ApproveOption: approve,
		RejectOption:  rejectName,
		ManifestHash:  hash,
		ExpiresAt:     expires.UTC().Format(time.RFC3339Nano),
		Presence:      "local_only",
	}
	if err := h.withRoot(func(root *fsq.DeliveryRoot) error { return h.appendEvent(root, bt.ref, bt, line) }); err != nil {
		return true
	}
	resolve := func(outcome string) {
		_ = h.withRoot(func(root *fsq.DeliveryRoot) error {
			return h.appendEvent(root, bt.ref, bt, bridgeEvent{Event: "interaction_resolved", InteractionID: id, Outcome: outcome, Option: rejectName})
		})
	}
	answerFile := bridgePath(h.cfg.Me, "answers", answerName(bt.ref, id))
	timer := time.NewTimer(time.Until(expires))
	defer timer.Stop()
	poll := time.NewTicker(answerPoll)
	defer poll.Stop()
	for {
		select {
		case <-turn.cancelled:
			return false
		case <-turn.done:
			return false
		case <-timer.C:
			resolve("answered_elsewhere")
			return true
		case <-poll.C:
			var data []byte
			if h.withRoot(func(root *fsq.DeliveryRoot) error {
				var err error
				data, err = root.ReadFile(answerFile)
				return err
			}) != nil {
				continue
			}
			var ans bridgeAnswer
			valid := json.Unmarshal(data, &ans) == nil && ans.Protocol == BridgeProtocol && ans.Ref == bt.ref &&
				ans.InteractionID == id && ans.ManifestHash == hash && ans.Option == rejectName
			if valid {
				resolve("answered")
			} else {
				// An answer the host cannot apply is refused; the policy
				// answers instead.
				resolve("answered_elsewhere")
			}
			return true
		}
	}
}

// approvalPrompt is what the owner sees: the tool call's title, then its raw
// input when the agent sent one.
func approvalPrompt(toolCall json.RawMessage) string {
	var call struct {
		Title    string          `json:"title"`
		RawInput json.RawMessage `json:"rawInput"`
	}
	_ = json.Unmarshal(toolCall, &call)
	text := strings.TrimSpace(call.Title)
	if len(call.RawInput) > 0 && string(call.RawInput) != "null" {
		text = strings.TrimSpace(text + "\n" + string(call.RawInput))
	}
	if text == "" {
		text = "a tool call"
	}
	return text
}

// answerName is the answer file for one approval: the lowercase hex SHA-256
// of the ref, a NUL byte, and the interaction id.
func answerName(ref, interactionID string) string {
	sum := sha256.Sum256([]byte(ref + "\x00" + interactionID))
	return hex.EncodeToString(sum[:]) + ".json"
}
