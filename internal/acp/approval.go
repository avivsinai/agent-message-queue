package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/secretscan"
)

// A native Claude turn driven from Buzz Desktop has no relay share. amq-acp
// shows each pending tool approval in the DM and relays the owner's ❌ as a
// deny. When the approval offers allow with proof (the Claude hook carries a
// Desktop pin), amq-acp posts it under the agent key, reads the owner's
// signed ✅ and the message's history from the relay itself, and hands the
// endpoint the reaction, the message and a grant signed with the agent key;
// the endpoint and the hook verify them against the hook's pin. The agent key
// never leaves this process.

// denyOnlyText follows every approval this path posts.
const denyOnlyText = "Buzz can deny this request only. ❌ denies; to allow, answer in Claude's terminal. ✅ here cannot approve."

// allowRefusedText answers an owner's ✅, once per approval.
const allowRefusedText = "Allow is not available from Buzz here; answer in Claude's terminal."

// deniedText says the owner's ❌ blocked the tool call.
const deniedText = "Denied from Buzz: the tool call was blocked."

// allowedText says the owner's ✅ let the tool call run.
const allowedText = "Allowed from Buzz: the tool call runs."

// alteredText answers a ✅ on an approval message that was edited or
// deleted after it was posted: it never allows.
const alteredText = "The approval message was altered after it was posted; check the terminal."

// mismatchText says the relay stored another text than the one posted, so a
// ✅ on it cannot prove the call.
const mismatchText = "This approval was stored differently from what AMQ posted, so ✅ cannot allow it; ❌ still denies, or answer in Claude's terminal."

// denyRetryText says a ❌ could not be handed to the session yet; AMQ keeps
// trying while the approval is pending.
const denyRetryText = "Your ❌ could not reach the session yet; AMQ keeps trying while the approval is pending."

// reactionPollInterval is the gap between reads of the owner's reactions on
// a posted approval: every few seconds while it is pending.
var reactionPollInterval = 3 * time.Second

// buzzOwner is the managed agent's owner pubkey (64 lowercase hex), or ""
// when it cannot be established. Only its reactions answer an approval.
var buzzOwner string

// agentOwner is the owner that Buzz Desktop gave this agent: the NIP-OA
// BUZZ_AUTH_TAG ["auth", owner, conditions, sig], verified against the
// agent's own key as buzz-acp verifies it (resolve_agent_owner), else
// BUZZ_ACP_AGENT_OWNER, which Desktop sets only for an agent with no auth
// tag. A tag that does not verify yields no owner.
func agentOwner(authTag, secret, fallback string) string {
	if authTag = strings.TrimSpace(authTag); authTag != "" {
		var tag []string
		if json.Unmarshal([]byte(authTag), &tag) != nil {
			return ""
		}
		t, err := bodykey.ParseAuthTag(tag)
		if err != nil {
			return ""
		}
		agent, err := agentPubKey(strings.TrimSpace(secret))
		if err != nil || t.Verify(agent) != nil {
			return ""
		}
		return t.OwnerPubKey
	}
	if fallback = strings.ToLower(strings.TrimSpace(fallback)); len(fallback) == 64 {
		if _, err := nostr.PubKeyFromHex(fallback); err == nil {
			return fallback
		}
	}
	return ""
}

// agentPubKey is the hex public key of a hex or nsec secret.
func agentPubKey(secret string) (string, error) {
	sk, err := agentSecret(secret)
	if err != nil {
		return "", err
	}
	return sk.Public().Hex(), nil
}

// agentSecret parses a hex or nsec secret.
func agentSecret(secret string) (nostr.SecretKey, error) {
	if strings.HasPrefix(secret, "nsec1") {
		prefix, val, err := nip19.Decode(secret)
		sk, ok := val.(nostr.SecretKey)
		if err != nil || prefix != "nsec" || !ok {
			return nostr.SecretKey{}, errors.New("invalid nsec")
		}
		return sk, nil
	}
	return nostr.SecretKeyFromHex(secret)
}

// buzzRelay is how the managed agent signs in to its relay to read the
// owner's signed reactions and an approval message's history, or nil when
// the relay URL, the secret or the NIP-OA auth tag is missing or invalid.
// Without it a Desktop approval is deny-only.
var buzzRelay *relay.Config

// agentRelay is the relay sign-in for a relay URL, the agent's secret and
// its BUZZ_AUTH_TAG JSON, or nil.
func agentRelay(url, secret, authTag string) *relay.Config {
	url, secret = strings.TrimSpace(url), strings.TrimSpace(secret)
	var tag []string
	if json.Unmarshal([]byte(strings.TrimSpace(authTag)), &tag) != nil || len(tag) != 4 || tag[0] != "auth" || relay.ValidateURL(url) != nil {
		return nil
	}
	sk, err := agentSecret(secret)
	if err != nil {
		return nil
	}
	return &relay.Config{URL: url, Secret: sk, AuthTag: tag}
}

// reaction is one emoji on a message and who reacted with it.
type reaction struct {
	Emoji   string   `json:"emoji"`
	PubKeys []string `json:"pubkeys"`
}

// readReactions reads the reactions on one event. A variable so tests can
// supply reactions without a relay.
var readReactions = reactionsWithBuzzCLI

// reactionsWithBuzzCLI runs `buzz reactions get --event <id>`, which prints
// {"reactions":[{"emoji","count","pubkeys"}]}.
func reactionsWithBuzzCLI(eventID string, budget time.Duration) ([]reaction, error) {
	out, err := runBuzzCLI(budget, nil, "reactions", "get", "--event", eventID)
	if err != nil {
		return nil, err
	}
	var got struct {
		Reactions []reaction `json:"reactions"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("buzz reactions get: %v", err)
	}
	return got.Reactions, nil
}

// reacted reports whether owner reacted with emoji.
func reacted(rs []reaction, owner, emoji string) bool {
	for _, r := range rs {
		if strings.TrimSpace(r.Emoji) == emoji && slices.Contains(r.PubKeys, owner) {
			return true
		}
	}
	return false
}

// approvalText is the DM message for one deny-only approval: the bounded
// preview, or "Command hidden" when it may hold a secret, then the
// deny-only note and hint, when there is one.
func approvalText(prompt, hint string) string {
	preview := protocol.BoundPreview(prompt)
	if secretscan.MayHold(preview) {
		preview = "Command hidden"
	}
	text := "Approval needed:\n\n" + preview + "\n\n" + denyOnlyText
	if hint != "" {
		text += "\n\n" + hint
	}
	return text
}

// desktopPinned reports whether the installed Claude approval hook pins this
// agent's DM: a Desktop pin with body and channel. A variable so tests can
// answer without a settings file.
var desktopPinned = func(body, channel string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	pin := claude.PermissionHookPin(home)
	return pin.DesktopComplete() && pin.Body == body && pin.Channel == channel
}

// desktopHint is the command that lets the owner allow from this DM, shown
// under a deny-only approval while the hook does not pin this agent's DM;
// "" when the agent cannot read its relay or the hook pins it already.
func desktopHint(channel string) string {
	if buzzRelay == nil || buzzOwner == "" || buzzSelf == "" || channel == "" || desktopPinned(buzzSelf, channel) {
		return ""
	}
	return "To allow from Buzz here, run in Claude's terminal: amq-remote claude install-approval-hook --desktop --owner " +
		buzzOwner + " --body " + buzzSelf + " --channel " + channel
}

// denyApproval reports whether in is an approval this path can show: a
// remote answer with a reject option.
func denyApproval(in *protocol.Interaction) bool {
	return in != nil && in.Kind == "approval" && in.RemoteAnswer && in.RejectOption != ""
}

// allowable reports whether this path can relay a ✅ for in: it offers allow
// with proof, the agent can read its relay, and the prompt is shown whole.
func allowable(in protocol.Interaction) bool {
	return in.ApproveOption != "" && in.ApproveProof && in.ApproveOption != in.RejectOption &&
		buzzRelay != nil && buzzOwner != "" && buzzSelf != "" &&
		protocol.BoundPreview(in.Prompt) == in.Prompt && !secretscan.MayHold(in.Prompt)
}

// sentAnswer is an answer this turn handed over and the line that reports it
// once the record shows it applied.
type sentAnswer struct{ option, text string }

// approvals follows the pending approvals of one remote turn. The follow
// loop calls observe with every snapshot; each new approval gets one
// watcher, which posts it once and polls the owner's reactions until the
// approval resolves or the turn ends.
type approvals struct {
	r       *remoteTurn
	channel string
	owner   string

	mu       sync.Mutex
	stops    map[string]chan struct{} // interaction id -> its watcher's stop
	sent     map[string]sentAnswer    // interaction id -> the answer this turn sent
	reported map[string]bool          // answers whose outcome was posted
	wg       sync.WaitGroup
}

func newApprovals(r *remoteTurn, channel, owner string) *approvals {
	return &approvals{r: r, channel: channel, owner: owner, stops: map[string]chan struct{}{}, sent: map[string]sentAnswer{}, reported: map[string]bool{}}
}

// observe starts a watcher for a new approval, retires watchers of
// approvals no longer pending, and reports an answer the record shows applied.
func (a *approvals) observe(snap protocol.Snapshot) {
	if a.channel == "" || a.owner == "" {
		return
	}
	a.mu.Lock()
	current := ""
	if in := snap.Interaction; denyApproval(in) {
		current = in.InteractionID
		if _, seen := a.stops[current]; !seen {
			stop := make(chan struct{})
			a.stops[current] = stop
			a.wg.Add(1)
			go a.watch(snap, *in, stop)
		}
	}
	for id, stop := range a.stops {
		if id != current && stop != nil {
			close(stop)
			a.stops[id] = nil
		}
	}
	lines := a.claimReportsLocked(snap)
	a.mu.Unlock()
	a.report(lines)
}

// claimReportsLocked claims, once each, the answers of this turn that the
// record shows applied with the option sent, and returns their lines.
func (a *approvals) claimReportsLocked(snap protocol.Snapshot) []string {
	var lines []string
	for _, res := range snap.Resolved {
		sent, ok := a.sent[res.InteractionID]
		if ok && !a.reported[res.InteractionID] && res.Outcome == protocol.ResolutionAnswered && res.Option == sent.option {
			a.reported[res.InteractionID] = true
			lines = append(lines, sent.text)
		}
	}
	return lines
}

// report posts the outcome lines. It runs outside a.mu: the lock is not held
// across the network.
func (a *approvals) report(lines []string) {
	for _, line := range lines {
		a.r.s.publish(a.channel, line)
	}
}

// close stops every watcher and waits for them.
func (a *approvals) close() {
	a.mu.Lock()
	for id, stop := range a.stops {
		if stop != nil {
			close(stop)
			a.stops[id] = nil
		}
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// watch posts one approval and relays the owner's ❌ as its reject option,
// and, for an approval this path can allow, the owner's proven ✅. A failed
// post or answer is retried on the next poll, so a network error never
// leaves the owner's answer unanswered; the approval is posted once, and the
// watcher ends when an answer is handed over or the approval is no longer
// pending (observe closes stop).
func (a *approvals) watch(snap protocol.Snapshot, in protocol.Interaction, stop chan struct{}) {
	defer a.wg.Done()
	if allowable(in) {
		w := &allowWatch{a: a, snap: snap, in: in, text: buzzio.DesktopApprovalText(in.Prompt), tried: map[string]bool{}}
		defer w.drop()
		a.poll(stop, w.post, w.poll)
		return
	}
	posted, allowAnswered, denyFailed := "", false, false
	post := func(budget time.Duration) bool {
		id, err := postAnswer(a.channel, approvalText(in.Prompt, desktopHint(a.channel)), budget)
		posted = id
		return err == nil
	}
	poll := func(budget time.Duration) bool {
		rs, err := readReactions(posted, budget)
		if err != nil {
			return false
		}
		if reacted(rs, a.owner, "❌") {
			return a.denyOnce(snap, in, &denyFailed)
		}
		if !allowAnswered && reacted(rs, a.owner, "✅") {
			allowAnswered = true
			a.r.s.publish(a.channel, allowRefusedText)
		}
		return false
	}
	a.poll(stop, post, poll)
}

// poll posts once, then polls every reactionPollInterval until poll reports
// the approval answered or stop closes. post reports whether it posted.
func (a *approvals) poll(stop chan struct{}, post, poll func(time.Duration) bool) {
	budget := a.r.s.cfg.PostTimeout
	posted := false
	for {
		select {
		case <-stop:
			return
		default:
		}
		if !posted {
			posted = post(budget)
		}
		if posted && poll(budget) {
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(reactionPollInterval):
		}
	}
}

// denyOnce hands over the deny and reports whether it was handed over; the
// first failure tells the owner AMQ keeps trying.
func (a *approvals) denyOnce(snap protocol.Snapshot, in protocol.Interaction, failed *bool) bool {
	if a.answer(snap, in, in.RejectOption, deniedText, nil) == nil {
		return true
	}
	if !*failed {
		*failed = true
		a.r.s.publish(a.channel, denyRetryText)
	}
	return false
}

// answer hands option over as the approval's answer, with evidence for an
// allow. already_resolved means another answer came first: handed over,
// nothing to report. Any other error is returned for the caller to retry or
// report.
func (a *approvals) answer(snap protocol.Snapshot, in protocol.Interaction, option, text string, evidence json.RawMessage) error {
	rep, err := a.r.callEvidence(&protocol.Command{
		Schema:        protocol.SchemaCommand,
		Op:            protocol.OpInteractionRespond,
		RequestRef:    snap.RequestRef,
		TargetID:      snap.TargetID,
		Epoch:         snap.Epoch,
		InteractionID: in.InteractionID,
		Option:        option,
	}, evidence)
	if hasCode(err, protocol.CodeAlreadyResolved) {
		return nil
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sent[in.InteractionID] = sentAnswer{option: option, text: text}
	lines := a.claimReportsLocked(rep.Snapshot)
	a.mu.Unlock()
	a.report(lines)
	return nil
}

// allowWatch is one approval that offers allow with proof. Every read goes
// through the agent's own relay connection, so each reaction and the message
// arrive signed: the owner's ❌ denies, and the owner's ✅ on the message as
// posted, with no edit or deletion since, is handed over with the agent's
// grant.
type allowWatch struct {
	a    *approvals
	snap protocol.Snapshot
	in   protocol.Interaction
	text string

	posted     string
	conn       *relay.Conn
	msg        *nostr.Event
	mismatch   bool            // the relay stored another message: deny-only
	altered    bool            // edited or deleted after posting: deny-only
	tried      map[string]bool // ✅ reaction ids already answered or refused
	denyFailed bool
}

func (w *allowWatch) post(budget time.Duration) bool {
	id, err := postAnswer(w.a.channel, w.text, budget)
	w.posted = id
	return err == nil
}

// drop closes the relay connection; the next poll signs in again.
func (w *allowWatch) drop() {
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
}

// poll reads the posted message once, then the owner's reactions on it, and
// reports whether an answer was handed over. A relay error drops the
// connection and waits for the next poll.
func (w *allowWatch) poll(budget time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if w.conn == nil {
		conn, err := relay.Connect(ctx, *buzzRelay)
		if err != nil {
			return false
		}
		w.conn = conn
	}
	if w.msg == nil && !w.mismatch {
		m, found, err := buzzio.ReadEvent(ctx, w.conn, w.posted)
		switch {
		case err != nil:
			w.drop()
			return false
		case !found:
		case m.Kind != buzzio.KindDM || m.PubKey.Hex() != buzzSelf || !inChannel(m, w.a.channel) || m.Content != w.text:
			w.mismatch = true
			w.a.r.s.publish(w.a.channel, mismatchText)
		default:
			w.msg = &m
		}
	}
	rs, err := buzzio.OwnerReactions(ctx, w.conn, w.posted, buzzOwner)
	if err != nil {
		w.drop()
		return false
	}
	var approve *nostr.Event
	for i := range rs {
		switch strings.TrimSpace(rs[i].Content) {
		case "❌":
			return w.a.denyOnce(w.snap, w.in, &w.denyFailed)
		case "✅":
			if !w.tried[rs[i].ID.Hex()] {
				approve = &rs[i]
			}
		}
	}
	if approve == nil || w.msg == nil || w.altered {
		return false
	}
	if err := buzzio.CheckDesktopHistory(ctx, w.conn, *w.msg, buzzOwner); err != nil {
		if errors.Is(err, buzzio.ErrAltered) {
			w.altered = true
			w.a.r.s.publish(w.a.channel, alteredText)
		} else {
			w.drop()
		}
		return false
	}
	grant, err := buzzio.SignDesktopGrant(buzzRelay.Secret, w.msg.ID.Hex(), approve.ID.Hex(), time.Now())
	if err != nil {
		return false
	}
	evidence, err := json.Marshal(buzzio.DesktopEvidence{Reaction: *approve, Message: *w.msg, Grant: grant})
	if err != nil {
		return false
	}
	err = w.a.answer(w.snap, w.in, w.in.ApproveOption, allowedText, evidence)
	var refusal *protocol.Refusal
	switch {
	case err == nil:
		return true
	case errors.As(err, &refusal):
		// Refused with a reason the owner can act on: this ✅ is spent, a
		// new one or a ❌ still answers.
		w.tried[approve.ID.Hex()] = true
		w.a.r.s.publish(w.a.channel, refusal.Message)
	}
	return false
}

// inChannel reports whether evt is posted in channel (its h tag).
func inChannel(evt nostr.Event, channel string) bool {
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "h" {
			return t[1] == channel
		}
	}
	return false
}
