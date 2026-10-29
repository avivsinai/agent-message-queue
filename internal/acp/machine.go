package acp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// The machine agent ("AMQ Remote machine" in its system prompt) is one Buzz
// agent per machine that serves every session. The worker that leads its
// identity makes a private channel for each live named session that has no
// running or idle agent of its own, adds the owner as an admin and welcomes
// them with a mention. A prompt reaches a session only from that session's
// channel (machineBinding). The records live under
// <state>/channels/<relay key>/<machine pubkey>/:
//   - <name>.json, the record of the binding called name;
//   - retired/<reg>.json, a record that was replaced or retired, which is
//     never routed and is reconciled and archived until it may be pruned;
//   - <name>.reset, an owner's request for a new channel (amq-remote channel
//     reset), applied by the leader.
//
// Only the leader writes records, under its lock, one step at a time: each
// outcome is written before the next step, and each step first re-reads the
// record. The buzz CLI makes a fresh channel id on every create and takes no
// idempotency key, so a create whose outcome is unknown, including one whose
// worker stopped before it wrote the result, is never sent again for that
// record; the leader looks for the channel it may have made instead. A
// record that exists but cannot be read stops all work on its name.

// machineName is the machine agent's marker directory under agents/. It is
// not a valid binding name, so it never meets a session's.
const machineName = "_machine"

// Channel record states.
const (
	channelCreating  = "creating"
	channelUncertain = "uncertain"
	channelActive    = "active"
	channelRetired   = "retired"
	channelFailed    = "failed"
)

// Why a record was retired.
const (
	retiredEnded    = "ended"    // its session is proven ended
	retiredDetached = "detached" // the binding is gone; its session may still run
	retiredMoved    = "moved"    // the same session attached again under another name
	retiredReset    = "reset"    // amq-remote channel reset
	retiredReplaced = "replaced" // another session identity holds the name
)

const (
	// machineScanInterval is how often the leader reads the bindings.
	machineScanInterval = 2 * time.Second
	// channelRetryGap separates creates after a refusal, lookups after an
	// uncertain create, and archive attempts.
	channelRetryGap = 10 * time.Minute
	// channelStepGap separates retries of add-member, the welcome and
	// unarchive.
	channelStepGap = 30 * time.Second
	// channelCreateTries bounds the refused creates of one record.
	channelCreateTries = 3
	// retiredKeep is how many retired records are kept before the prunable
	// ones are removed, the oldest first.
	retiredKeep = 256
)

// buzzRelayKey is the sha256 of the normalized BUZZ_RELAY_URL captured at
// worker start, and buzzRelayURL that URL; both are "" without a usable URL,
// and the machine agent then provisions nothing.
var buzzRelayKey, buzzRelayURL string

// normalizeRelay is a relay URL with its scheme and host lowercased and no
// trailing slash, query or fragment; "" when it is not an absolute URL.
func normalizeRelay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

// relayKey is the directory key of a relay URL.
func relayKey(raw string) string {
	norm := normalizeRelay(raw)
	if norm == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// channelRecord is the machine agent's record of one session's channel.
type channelRecord struct {
	// The route: the relay, the machine agent's key and the channel.
	Relay     string `json:"relay"`
	Machine   string `json:"machine"`
	ChannelID string `json:"channel_id,omitempty"`
	// The session: the binding's name and its SessionIdentity.
	Name     string `json:"name"`
	Identity string `json:"identity"`
	// Label is the channel's name in Buzz, fixed when the record is made.
	Label string `json:"label"`
	// Reg is made once per record and written into the channel's
	// description, so an uncertain create can be found.
	Reg        string `json:"reg"`
	State      string `json:"state"`
	Archived   bool   `json:"archived"`
	OwnerAdded bool   `json:"owner_added"`
	Welcomed   bool   `json:"welcomed"`
	Creates    int    `json:"creates"`
	// Pending is written before a create is sent and cleared with its
	// result, so a create whose worker stopped in between stays unresolved.
	Pending bool `json:"pending,omitempty"`
	// Doubted records that a create outcome was once unknown: no create is
	// ever sent again for this record.
	Doubted bool `json:"doubted,omitempty"`
	// ArchiveSent is written before the channel's archive is sent, so a
	// restore unarchives a channel that may be archived although no archive
	// was confirmed.
	ArchiveSent bool `json:"archive_sent,omitempty"`
	// Orphans are channels found for Reg that are not used: both of an
	// ambiguous match. A retired record archives them.
	Orphans     []string `json:"orphans,omitempty"`
	RetiredBy   string   `json:"retired_by,omitempty"`
	RetiredFrom string   `json:"retired_from,omitempty"`
	SaidEnded   bool     `json:"said_ended,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	LastTry     string   `json:"last_try,omitempty"`
	Updated     string   `json:"updated"`
}

// inDoubt reports whether a create of r may have made a channel that is not
// known yet.
func (r channelRecord) inDoubt() bool {
	return (r.Doubted || r.Pending) && r.ChannelID == "" && len(r.Orphans) == 0
}

// prunable reports whether a retired record may be removed: every channel it
// made is archived, or it made none and no create is in doubt.
func (r channelRecord) prunable() bool {
	return r.Archived || (r.ChannelID == "" && len(r.Orphans) == 0 && !r.inDoubt())
}

func channelDirFor(stateDir, relayKey, machine string) string {
	return filepath.Join(stateDir, "channels", relayKey, machine)
}

func (s *Server) channelDir() string { return channelDirFor(s.cfg.StateDir, buzzRelayKey, buzzSelf) }

func (s *Server) retiredDir() string { return filepath.Join(s.channelDir(), "retired") }

// loadChannelRecord reads one record. found is false only when there is no
// file; a file that cannot be read or parsed is an error, never first use.
func loadChannelRecord(path string) (r channelRecord, found bool, err error) {
	raw, err := readSmallRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return channelRecord{}, false, nil
	}
	if err != nil {
		return channelRecord{}, true, err
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Reg == "" {
		return channelRecord{}, true, fmt.Errorf("unreadable channel record %s", path)
	}
	return r, true, nil
}

// readChannelRecord is loadChannelRecord for readers that act only on a
// good record.
func readChannelRecord(path string) (channelRecord, bool) {
	r, found, err := loadChannelRecord(path)
	return r, found && err == nil
}

// recordPath is where r lives: the active slot of its name, or the retired
// store once it is retired.
func (s *Server) recordPath(r channelRecord) string {
	if r.State == channelRetired {
		return filepath.Join(s.retiredDir(), r.Reg+".json")
	}
	return s.slotPath(r.Name)
}

func (s *Server) slotPath(name string) string { return filepath.Join(s.channelDir(), name+".json") }

// save writes r atomically where it lives.
func (s *Server) save(r *channelRecord, now time.Time) error {
	r.Updated = now.UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := s.recordPath(*r)
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), append(raw, '\n'), 0o600)
	return err
}

// unchanged reports whether the stored record still has r's identity, reg
// and state, so a step never acts on a record that moved on.
func (s *Server) unchanged(r channelRecord) bool {
	cur, ok := readChannelRecord(s.recordPath(r))
	return ok && cur.Identity == r.Identity && cur.Reg == r.Reg && cur.State == r.State
}

func due(last string, gap time.Duration, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, last)
	return err != nil || now.Sub(at) >= gap
}

func newReg() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// errMachineStopped refuses a relay call after the machine worker stopped.
var errMachineStopped = errors.New("the machine agent's worker stopped")

// stopped reports whether the machine worker is stopping: no new step and no
// new record starts then.
func (s *Server) stopped() bool {
	return s.machine.ctx != nil && s.machine.ctx.Err() != nil
}

// call runs one buzz command for the machine lifecycle. A call cut off by
// the worker's stop fails like a timeout: its outcome is unknown.
func (s *Server) call(args ...string) ([]byte, error) {
	ctx := s.machine.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, &notSentError{errMachineStopped}
	}
	return machineCall(ctx, s.cfg.PostTimeout, args...)
}

// machineTick runs every machineScanInterval in the worker that leads the
// machine identity, until ctx ends. It writes the marker first, before any
// relay call, then applies reset requests, retires the records of bindings
// that are gone, provisions the live sessions, and every lateReplyInterval
// works through the retired store.
func (s *Server) machineTick(ctx context.Context, now time.Time) {
	s.machine.ctx = ctx
	if !s.cfg.Machine || buzzSelf == "" || s.stopped() || !s.lead(machineName) {
		return
	}
	if due(s.machine.marked, lateReplyInterval, now) {
		dir := s.agentMarkerDir(machineName)
		m, _ := readAgentMarker(filepath.Join(dir, buzzSelf+".json"))
		m.Name, m.DisplayName, m.RelayKey = machineName, buzzAgent.displayName, buzzRelayKey
		s.writeAgentMarker(dir, m, now)
		s.machine.marked = now.UTC().Format(time.RFC3339)
	}
	if buzzRelayKey == "" || buzzOwner == "" {
		return
	}
	s.applyResets(now)
	s.retireGone(now)
	all, err := binding.List()
	if err == nil {
		for _, b := range all {
			if s.stopped() {
				return
			}
			if s.provisionable(b) {
				s.provision(b, now)
			}
		}
	}
	if !s.stopped() && due(s.machine.swept, lateReplyInterval, now) {
		s.machine.swept = now.UTC().Format(time.RFC3339)
		s.sweepRetired(now)
	}
}

// machineState is the machine worker's own state: its context and pacing.
type machineState struct {
	ctx           context.Context
	marked, swept string
}

// provisionable reports whether the machine agent serves b: a named binding
// whose session is proven live and that has no agent of its own that runs
// or is idle. An offline marker, or one without an identity that is stale,
// is what a deleted Desktop agent leaves, so it does not keep the channel
// away.
func (s *Server) provisionable(b binding.Binding) bool {
	if b.Name == binding.LegacyName || binding.ValidName(b.Name) != nil {
		return false
	}
	if state, _ := b.Liveness(); state != binding.Live {
		return false
	}
	for _, m := range s.agentMarkers(b.Name) {
		if state := m.state(time.Now()); state == AgentRunning || state == AgentIdle {
			return false
		}
	}
	return true
}

// provision moves b's record one or more steps toward active. A record that
// cannot be read, or one that could not be moved to the retired store,
// stops it: a new record never replaces state it could not keep.
func (s *Server) provision(b binding.Binding, now time.Time) {
	r, ok, err := loadChannelRecord(s.slotPath(b.Name))
	if err != nil {
		return
	}
	if ok && r.State == channelRetired {
		if _, moved := s.finishRetire(r, now); !moved {
			return
		}
		ok = false
	}
	if ok && r.Identity != b.SessionIdentity() {
		if !s.retire(r, retiredReplaced, now) {
			return
		}
		ok = false
	}
	if !ok {
		if s.restore(b, now) || s.stopped() {
			return
		}
		reg, err := newReg()
		if err != nil {
			return
		}
		r = channelRecord{Relay: normalizeRelay(buzzRelayURL), Machine: buzzSelf, Name: b.Name, Identity: b.SessionIdentity(),
			Label: sessionAgentName(b), Reg: reg, State: channelCreating}
		if s.save(&r, now) != nil {
			return
		}
	}
	if r.Pending {
		// A create was sent and its result never written: the worker
		// stopped while it ran.
		r.State, r.Pending, r.Doubted, r.LastError = channelUncertain, false, true, "the outcome of a create was not recorded"
		_ = s.save(&r, now)
		return
	}
	switch r.State {
	case channelActive:
		return
	case channelCreating:
	case channelFailed:
		if r.Doubted || r.Creates >= channelCreateTries || !due(r.LastTry, channelRetryGap, now) {
			return
		}
	case channelUncertain:
		if !due(r.LastTry, channelRetryGap, now) || !s.lookup(&r, now) {
			return
		}
	default:
		return
	}
	if r.ChannelID == "" && !s.create(&r, now) {
		return
	}
	s.finishSetup(&r, now)
}

// stillServes re-reads r and its binding before a step. A binding proven gone
// or ended retires r; another identity under the name replaces it; any other
// read error proves nothing and only skips the step. Nothing starts once the
// worker stops.
func (s *Server) stillServes(r channelRecord, now time.Time) bool {
	if s.stopped() || !s.unchanged(r) {
		return false
	}
	b, err := binding.ReadNamed(r.Name)
	switch {
	case errors.Is(err, binding.ErrNone):
		s.retire(r, goneReason(r), now)
		return false
	case err != nil:
		return false
	case b.SessionIdentity() != r.Identity:
		s.retire(r, retiredReplaced, now)
		return false
	}
	if state, _ := b.Liveness(); state == binding.Ended {
		s.retire(r, retiredEnded, now)
		return false
	}
	return true
}

// createOutcome is what one channels create proves.
type createOutcome int

const (
	createAccepted createOutcome = iota
	createRefused
	createUncertain
)

// classifyCreate reads a create's result. Accepted needs exit 0 with
// accepted:true and a channel id. Only a refusal the relay gave (exit 1 bad
// input, exit 3 auth, or accepted:false), or a CLI that never started, proves
// that no channel was made; anything else may have made one.
func classifyCreate(out []byte, err error) (createOutcome, string, string) {
	if err != nil {
		var exit *buzzExitError
		var notSent *notSentError
		switch {
		case errors.As(err, &notSent), errors.Is(err, errNoBuzzIdentity):
			return createRefused, "", err.Error()
		case errors.As(err, &exit) && (exit.code == 1 || exit.code == 3):
			return createRefused, "", err.Error()
		}
		return createUncertain, "", err.Error()
	}
	var created struct {
		Accepted  *bool  `json:"accepted"`
		ChannelID string `json:"channel_id"`
	}
	switch {
	case json.Unmarshal(out, &created) != nil || created.Accepted == nil:
		return createUncertain, "", "buzz channels create: unreadable output"
	case !*created.Accepted:
		return createRefused, "", "buzz channels create: the relay refused it: " + strings.TrimSpace(string(out))
	case !uuidRe.MatchString(created.ChannelID):
		return createUncertain, "", "buzz channels create: no channel id in its output"
	}
	return createAccepted, strings.ToLower(created.ChannelID), ""
}

// create counts and sends one channels create. The count and the pending
// mark are written before the call, and the outcome after it, so a worker
// that stops in between leaves the create unresolved. It reports whether a
// channel id is known.
func (s *Server) create(r *channelRecord, now time.Time) bool {
	if r.Doubted || r.Creates >= channelCreateTries || !s.stillServes(*r, now) {
		return false
	}
	r.Creates++
	r.State, r.Pending, r.LastTry = channelCreating, true, now.UTC().Format(time.RFC3339)
	if s.save(r, now) != nil {
		return false
	}
	out, err := s.call("channels", "create", "--name", r.Label, "--type", "stream",
		"--visibility", "private", "--description", "amq-remote "+r.Reg)
	outcome, id, reason := classifyCreate(out, err)
	r.Pending = false
	switch outcome {
	case createAccepted:
		r.ChannelID, r.LastError = id, ""
	case createRefused:
		r.State, r.LastError = channelFailed, reason
	default:
		r.State, r.Doubted, r.LastError = channelUncertain, true, reason
	}
	if s.save(r, now) != nil {
		return false
	}
	return outcome == createAccepted
}

// lookup looks for the channel an uncertain create of r may have made, and
// adopts it when there is exactly one. Two are both refused. It reports
// whether r now has a channel to set up.
func (s *Server) lookup(r *channelRecord, now time.Time) bool {
	if !s.stillServes(*r, now) {
		return false
	}
	r.LastTry = now.UTC().Format(time.RFC3339)
	ids, ok := s.findChannels(*r)
	switch {
	case !ok:
	case len(ids) == 1:
		r.ChannelID, r.State, r.LastError = ids[0], channelCreating, ""
	case len(ids) > 1:
		r.State, r.Orphans = channelFailed, ids
		r.LastError = "several channels carry this registration (" + strings.Join(ids, ", ") + "); none is used"
	}
	return s.save(r, now) == nil && r.ChannelID != ""
}

// findChannels lists the private channels the machine agent belongs to and
// returns those named r.Label that it owns and whose description holds
// r.Reg. The list does not promise a description, so each candidate is read
// with channels get. ok is false when any read failed, which proves nothing.
func (s *Server) findChannels(r channelRecord) (ids []string, ok bool) {
	out, err := s.call("channels", "list", "--member", "--visibility", "private")
	var listed []struct {
		ChannelID string `json:"channel_id"`
		Name      string `json:"name"`
	}
	if err != nil || json.Unmarshal(out, &listed) != nil {
		return nil, false
	}
	for _, c := range listed {
		if c.Name != r.Label || !uuidRe.MatchString(c.ChannelID) {
			continue
		}
		out, err := s.call("channels", "get", "--channel", c.ChannelID)
		var got struct {
			Description string `json:"description"`
		}
		if err != nil || json.Unmarshal(out, &got) != nil {
			return nil, false
		}
		if !strings.Contains(got.Description, r.Reg) {
			continue
		}
		owner, known := ownerOf(s.call("channels", "members", "--channel", c.ChannelID))
		if !known {
			return nil, false
		}
		if owner {
			ids = append(ids, strings.ToLower(c.ChannelID))
		}
	}
	return ids, true
}

// accepted reports whether a write command succeeded: exit 0 and the relay
// said accepted:true. An event id alone is no acceptance: the CLI prints the
// event id of a write the relay refused too.
func accepted(out []byte, err error) error {
	if err != nil {
		return err
	}
	var res struct {
		Accepted bool `json:"accepted"`
	}
	if json.Unmarshal(out, &res) != nil || !res.Accepted {
		return fmt.Errorf("not accepted: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// finishSetup adds the owner, welcomes them with a mention so the phone
// pushes, and activates the channel. Each step's outcome is written before
// the next; a failed step is retried after channelStepGap.
func (s *Server) finishSetup(r *channelRecord, now time.Time) {
	if r.State != channelCreating || r.ChannelID == "" {
		return
	}
	if r.LastError != "" && !due(r.LastTry, channelStepGap, now) {
		return
	}
	step := func(done *bool, args ...string) bool {
		if *done {
			return true
		}
		if !s.stillServes(*r, now) {
			return false
		}
		r.LastTry = now.UTC().Format(time.RFC3339)
		if err := accepted(s.call(args...)); err != nil {
			r.LastError = err.Error()
			_ = s.save(r, now)
			return false
		}
		*done, r.LastError = true, ""
		return s.save(r, now) == nil
	}
	if !step(&r.OwnerAdded, "channels", "add-member", "--channel", r.ChannelID, "--pubkey", buzzOwner, "--role", "admin") {
		return
	}
	welcome := "AMQ Remote: this channel reaches " + r.Label + ". Write here to talk to that session."
	if !step(&r.Welcomed, "messages", "send", "--channel", r.ChannelID, "--content", welcome, "--mention", buzzOwner) {
		return
	}
	if !s.stillServes(*r, now) {
		return
	}
	r.State = channelActive
	_ = s.save(r, now)
}

// retire stops routing to r at once, moves it to the retired store, and
// starts its remote cleanup. It reports whether r is durably in the retired
// store, so its name may take a new record.
func (s *Server) retire(r channelRecord, by string, now time.Time) bool {
	fromSlot := r.State != channelRetired
	if fromSlot {
		r.RetiredFrom = r.State
	}
	r.State, r.RetiredBy = channelRetired, by
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return false
	}
	// A retired copy of this reg beside a record that is not retired is the
	// leftover of a restore that stopped half way: the record in the slot is
	// newer, so the leftover goes before the move. After that, a retired
	// copy of the reg can only be the move's own, at least as new as the
	// slot (finishRetire).
	if fromSlot {
		if err := s.removeDurably(filepath.Join(s.retiredDir(), r.Reg+".json")); err != nil {
			return false
		}
	}
	// The active slot says retired first, so routing stops before anything
	// else happens.
	slot := s.slotPath(r.Name)
	if _, err := fsq.WriteFileAtomic(filepath.Dir(slot), filepath.Base(slot), append(raw, '\n'), 0o600); err != nil {
		return false
	}
	moved, ok := s.finishRetire(r, now)
	if !ok {
		return false
	}
	s.cleanRetired(moved, now)
	return true
}

// finishRetire moves a retired record from its name's slot to the retired
// store, so the name can take a new record, and returns the retired record.
// When the retired store already holds the reg, that copy is the newer one
// (the slot came back after a stop before its removal was durable), so it
// is kept and only the slot is removed. The slot's removal is synced before
// it reports success.
func (s *Server) finishRetire(r channelRecord, now time.Time) (channelRecord, bool) {
	stored, found, err := loadChannelRecord(filepath.Join(s.retiredDir(), r.Reg+".json"))
	switch {
	case err != nil:
		return channelRecord{}, false
	case found && stored.Reg == r.Reg && stored.State == channelRetired:
		r = stored
	default:
		if s.save(&r, now) != nil {
			return channelRecord{}, false
		}
	}
	slot := s.slotPath(r.Name)
	if cur, ok := readChannelRecord(slot); ok && cur.Reg == r.Reg {
		if s.removeDurably(slot) != nil {
			return channelRecord{}, false
		}
	}
	return r, true
}

// removeDurably removes path, if it exists, and syncs its directory. An
// absent path is synced too: an earlier unlink whose sync failed is not yet
// durable, and a stop could bring the file back. Only a directory that was
// never created needs no sync.
func (s *Server) removeDurably(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err != nil {
		if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
	}
	return fsq.SyncDir(dir)
}

// goneReason is why the binding of r is gone, as the tombstone of r's
// session records it. A tombstone without a reason counts as ended only
// when its session is proven ended, else as detached, because a detach
// leaves the session running.
func goneReason(r channelRecord) string {
	tombs, _ := binding.Tombstones()
	for _, t := range tombs {
		if t.Name != r.Name || t.SessionIdentity() != r.Identity {
			continue
		}
		switch t.Removed {
		case binding.RemovedEnded:
			return retiredEnded
		case binding.RemovedMoved:
			return retiredMoved
		case binding.RemovedDetached:
			return retiredDetached
		}
		if state, _ := t.Liveness(); state == binding.Ended {
			return retiredEnded
		}
	}
	return retiredDetached
}

// left reports whether r was retired because its session left: it ended or
// it detached. The same session attaching again gets that channel back.
func left(r channelRecord) bool {
	return r.RetiredBy == retiredEnded || r.RetiredBy == retiredDetached
}

// retireGone retires each record whose binding is proven gone or ended, and
// finishes the move of a record a stopped worker left half retired.
func (s *Server) retireGone(now time.Time) {
	for _, r := range s.activeRecords() {
		if s.stopped() {
			return
		}
		if r.State == channelRetired {
			s.finishRetire(r, now)
			continue
		}
		b, err := binding.ReadNamed(r.Name)
		if errors.Is(err, binding.ErrNone) {
			s.retire(r, goneReason(r), now)
		} else if err == nil {
			if state, _ := b.Liveness(); state == binding.Ended {
				s.retire(r, retiredEnded, now)
			}
		}
	}
}

// activeRecords reads every good record in the name slots.
func (s *Server) activeRecords() []channelRecord {
	entries, _ := os.ReadDir(s.channelDir())
	var out []channelRecord
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			if r, ok := readChannelRecord(filepath.Join(s.channelDir(), e.Name())); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// retiredRecords reads the retired store.
func (s *Server) retiredRecords() []channelRecord {
	entries, _ := os.ReadDir(s.retiredDir())
	var out []channelRecord
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			if r, ok := readChannelRecord(filepath.Join(s.retiredDir(), e.Name())); ok && r.State == channelRetired {
				out = append(out, r)
			}
		}
	}
	return out
}

// cleanRetired works on one retired record: it finds a channel an uncertain
// or unfinished create made, says "Session ended" in a channel that was
// active for a session that ended, and archives every channel the record
// made. archived is written only after the archive is confirmed; a failure
// is retried after channelRetryGap without limit. A retired record never
// gets add-member, a welcome or activation.
func (s *Server) cleanRetired(r channelRecord, now time.Time) {
	if r.Archived || s.stopped() || (r.LastTry != "" && r.LastError != "" && !due(r.LastTry, channelRetryGap, now)) {
		return
	}
	if r.ChannelID == "" && len(r.Orphans) == 0 && !r.inDoubt() {
		return
	}
	if !s.unchanged(r) {
		return
	}
	r.LastTry = now.UTC().Format(time.RFC3339)
	fail := func(err error) {
		r.LastError = err.Error()
		_ = s.save(&r, now)
	}
	if r.inDoubt() {
		ids, ok := s.findChannels(r)
		if !ok {
			fail(errors.New("could not look for the channel of an uncertain create"))
			return
		}
		if len(ids) == 0 {
			r.LastError = "no channel found yet for an uncertain create"
			_ = s.save(&r, now)
			return
		}
		r.ChannelID, r.Orphans, r.Pending, r.Doubted = ids[0], ids[1:], false, true
		if s.save(&r, now) != nil {
			return
		}
	}
	if left(r) && r.RetiredFrom == channelActive && !r.SaidEnded && r.ChannelID != "" && !s.stopped() {
		r.SaidEnded = true
		if s.save(&r, now) != nil {
			return
		}
		text := "Session ended: " + r.Label + ". This channel no longer reaches it."
		if r.RetiredBy == retiredDetached {
			text = "Disconnected: " + r.Label + ". This channel no longer reaches it until the session connects again."
		}
		_ = accepted(s.call("messages", "send", "--channel", r.ChannelID, "--content", text))
	}
	for len(r.Orphans) > 0 {
		if s.stopped() {
			return
		}
		if err := accepted(s.call("channels", "archive", "--channel", r.Orphans[0])); err != nil {
			fail(err)
			return
		}
		r.Orphans = r.Orphans[1:]
		if s.save(&r, now) != nil {
			return
		}
	}
	if r.ChannelID != "" {
		if s.stopped() {
			return
		}
		r.ArchiveSent = true
		if s.save(&r, now) != nil {
			return
		}
		if err := accepted(s.call("channels", "archive", "--channel", r.ChannelID)); err != nil {
			fail(err)
			return
		}
	}
	r.Archived, r.LastError = true, ""
	_ = s.save(&r, now)
}

// sweepRetired cleans every retired record, drops a leftover of a restore
// that stopped half way, and prunes the oldest prunable records beyond
// retiredKeep. A record in doubt is never pruned.
func (s *Server) sweepRetired(now time.Time) {
	active := map[string]bool{}
	for _, r := range s.activeRecords() {
		if r.State != channelRetired {
			active[r.Reg] = true
		}
	}
	retired := s.retiredRecords()
	for _, r := range retired {
		if s.stopped() {
			return
		}
		if active[r.Reg] {
			_ = s.removeDurably(s.recordPath(r))
			continue
		}
		s.cleanRetired(r, now)
	}
	retired = s.retiredRecords()
	if len(retired) <= retiredKeep {
		return
	}
	sort.Slice(retired, func(i, j int) bool { return retired[i].Updated < retired[j].Updated })
	excess := len(retired) - retiredKeep
	for _, r := range retired {
		if excess == 0 {
			break
		}
		if r.prunable() {
			if os.Remove(s.recordPath(r)) == nil {
				excess--
			}
		}
	}
}

// restorable re-reads what a restore of r for b needs: the binding still
// names b's session and is not proven ended, the retired record is
// unchanged, and no record holds the name's slot.
func (s *Server) restorable(b binding.Binding, r channelRecord) bool {
	if s.stopped() || !s.unchanged(r) {
		return false
	}
	cur, err := binding.ReadNamed(b.Name)
	if err != nil || cur.SessionIdentity() != r.Identity {
		return false
	}
	if state, _ := cur.Liveness(); state == binding.Ended {
		return false
	}
	_, held, err := loadChannelRecord(s.slotPath(b.Name))
	return err == nil && !held
}

// restore brings back the channel of the same session attaching again: the
// newest retired record of b's name and identity that was active and ended
// with its session. A channel whose archive was sent is unarchived first,
// and only a confirmed unarchive lets it become active. It reports whether
// such a record exists, so no new channel is made while it is being brought
// back.
func (s *Server) restore(b binding.Binding, now time.Time) bool {
	var found *channelRecord
	for _, r := range s.retiredRecords() {
		if r.Name == b.Name && r.Identity == b.SessionIdentity() && left(r) &&
			r.RetiredFrom == channelActive && r.ChannelID != "" && (found == nil || r.Updated > found.Updated) {
			found = &r
		}
	}
	if found == nil {
		return false
	}
	r := *found
	if !s.restorable(b, r) {
		return true
	}
	if r.Archived || r.ArchiveSent {
		if r.LastError != "" && !due(r.LastTry, channelStepGap, now) {
			return true
		}
		r.LastTry = now.UTC().Format(time.RFC3339)
		if err := accepted(s.call("channels", "unarchive", "--channel", r.ChannelID)); err != nil {
			r.LastError = err.Error()
			_ = s.save(&r, now)
			return true
		}
		// The channel is unarchived now; if the session left meanwhile, the
		// retired record archives it again.
		r.Archived, r.ArchiveSent, r.LastError = false, false, ""
		if s.save(&r, now) != nil || !s.restorable(b, r) {
			return true
		}
	}
	retiredPath := s.recordPath(r)
	r.State, r.RetiredBy, r.RetiredFrom, r.SaidEnded, r.LastError = channelActive, "", "", false, ""
	if s.save(&r, now) != nil {
		return true
	}
	// A leftover retired copy (a stop before this removal is durable) is
	// dropped by sweepRetired: the active record of its reg supersedes it.
	_ = s.removeDurably(retiredPath)
	_ = accepted(s.call("messages", "send", "--channel", r.ChannelID, "--content", "Connected again: "+r.Label+"."))
	return true
}

// resetRequest asks the leader to retire the record with Reg and make a
// new one. It is written by amq-remote channel reset beside the record.
type resetRequest struct {
	Reg string `json:"reg"`
}

// applyResets retires each record named by a reset request whose reg still
// matches it. The next provisioning makes a new record with a new reg; the
// retired one keeps being reconciled and archived, so a late channel of an
// uncertain create is archived as an orphan. A request is dropped once it is
// applied or names no current record; one whose request or record cannot be
// read, or whose retirement failed, stays for the next scan.
func (s *Server) applyResets(now time.Time) {
	matches, _ := filepath.Glob(filepath.Join(s.channelDir(), "*.reset"))
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), ".reset")
		var req resetRequest
		raw, err := readSmallRegular(path)
		if err != nil || json.Unmarshal(raw, &req) != nil {
			continue
		}
		r, found, err := loadChannelRecord(s.slotPath(name))
		switch {
		case err != nil:
			continue
		case found && r.Reg == req.Reg && r.State != channelRetired:
			if !s.retire(r, retiredReset, now) {
				continue
			}
		}
		_ = os.Remove(path)
	}
}

// channelRoute returns the active record of channel under this agent's own
// relay and key directory. A record anywhere else is never read for routing.
func (s *Server) channelRoute(channel string) (channelRecord, bool) {
	if channel == "" || buzzRelayKey == "" || buzzSelf == "" {
		return channelRecord{}, false
	}
	for _, r := range s.activeRecords() {
		if r.ChannelID == channel && r.State == channelActive && r.Machine == buzzSelf && r.Relay == normalizeRelay(buzzRelayURL) {
			return r, true
		}
	}
	return channelRecord{}, false
}

// errChannelNotConnected refuses a prompt from a channel whose record names
// a session identity that the binding no longer has.
var errChannelNotConnected = errors.New("this channel's session is not connected")

// machineBinding is the binding a machine agent's ACP session drives: the
// session named by the active record of the channel the session's first
// prompt came from. The current prompt must carry that same channel. It
// never uses a pin, a selected model or the only binding; anything that does
// not resolve gets the directory.
func (s *Server) machineBinding(sessionID string) (binding.Binding, error) {
	s.mu.Lock()
	route, current := "", ""
	if session, ok := s.sessions[sessionID]; ok {
		route = session.route
		if session.turn != nil {
			current = session.turn.route
		}
	}
	s.mu.Unlock()
	if route == "" || current != route {
		return binding.Binding{}, &unpinnedError{machine: true}
	}
	r, ok := s.channelRoute(route)
	if !ok {
		return binding.Binding{}, &unpinnedError{machine: true}
	}
	b, err := binding.ReadNamed(r.Name)
	switch {
	case errors.Is(err, binding.ErrNone):
		return binding.Binding{}, errChannelNotConnected
	case err != nil:
		return binding.Binding{}, err
	case b.SessionIdentity() != r.Identity:
		return binding.Binding{}, errChannelNotConnected
	}
	return b, nil
}

// machineDirectory is the machine agent's answer to a prompt that reaches
// no session: each live session with where to reach it.
func (s *Server) machineDirectory() string {
	var b strings.Builder
	name := "AMQ Remote"
	if m := binding.Machine(); m != "" {
		name += " @ " + m
	}
	fmt.Fprintf(&b, "This is %s; it runs prompts only in a session's own channel.", name)
	records := map[string]channelRecord{}
	for _, r := range s.activeRecords() {
		records[r.Name] = r
	}
	all, _ := binding.List()
	listed := 0
	for _, bd := range all {
		if state, _ := bd.Liveness(); state != binding.Live || bd.Name == binding.LegacyName {
			continue
		}
		if listed == 0 {
			b.WriteString(" Sessions connected on this machine:")
		}
		listed++
		label := sessionAgentName(bd)
		r, ok := records[bd.Name]
		switch {
		case ok && r.State == channelActive && r.Identity == bd.SessionIdentity():
			fmt.Fprintf(&b, "\n- %s: write in the %q channel", label, r.Label)
		case !s.provisionable(bd):
			fmt.Fprintf(&b, "\n- %s: DM its own agent", label)
		case ok:
			fmt.Fprintf(&b, "\n- %s: its channel is %s", label, r.State)
		default:
			fmt.Fprintf(&b, "\n- %s: its channel is being made", label)
		}
	}
	if listed == 0 {
		b.WriteString(" No session is connected.")
	}
	b.WriteString("\nRun /amq-remote in a session to connect it.")
	return b.String()
}

// MachineChannel is the machine agent's channel record of one session, as
// amq-remote reports it.
type MachineChannel struct {
	// State is creating, uncertain, active, failed or retired.
	State string
	// Name is the channel's name in Buzz.
	Name  string
	Error string
	// Retry is true for a failed record that AMQ creates again after
	// channelRetryGap; otherwise only channel reset makes a new channel.
	Retry    bool
	reg      string
	dir      string
	identity string
}

// BindingStateDir is amq-acp's state directory in binding mode, beside the
// binding file.
func BindingStateDir() (string, error) {
	path, err := binding.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "acp"), nil
}

// machineDirOf is the record directory of this machine's newest machine
// agent: the relay key and key its newest marker names.
func machineDirOf(stateDir string) (string, bool) {
	dir := filepath.Join(stateDir, "agents", machineName)
	entries, _ := os.ReadDir(dir)
	var newest agentMarker
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		m, ok := readAgentMarker(filepath.Join(dir, e.Name()))
		if ok && m.RelayKey != "" && len(m.PubKey) == 64 && m.Updated > newest.Updated {
			newest = m
		}
	}
	if newest.PubKey == "" {
		return "", false
	}
	return channelDirFor(stateDir, newest.RelayKey, newest.PubKey), true
}

// MachineChannelOf reads the channel record of b that this machine's
// machine agent keeps; false when there is none, or when the record under
// b's name is another session's.
func MachineChannelOf(b binding.Binding) (MachineChannel, bool) {
	c, ok := machineChannel(b.Name)
	if !ok || c.identity != b.SessionIdentity() {
		return MachineChannel{}, false
	}
	return c, true
}

func machineChannel(name string) (MachineChannel, bool) {
	stateDir, err := BindingStateDir()
	if err != nil {
		return MachineChannel{}, false
	}
	dir, ok := machineDirOf(stateDir)
	if !ok {
		return MachineChannel{}, false
	}
	r, ok := readChannelRecord(filepath.Join(dir, name+".json"))
	if !ok {
		return MachineChannel{}, false
	}
	c := MachineChannel{State: r.State, Name: r.Label, reg: r.Reg, dir: dir, identity: r.Identity}
	if r.State == channelUncertain || r.State == channelFailed {
		c.Error = r.LastError
		c.Retry = r.State == channelFailed && !r.Doubted && r.Creates < channelCreateTries
	}
	return c, true
}

// RequestChannelReset asks the machine agent to give up the channel record
// of the binding called name and make a new channel. The machine agent
// applies it on its next scan; the old record stays in the retired store,
// where a channel an uncertain create made is still found and archived.
func RequestChannelReset(name string) (MachineChannel, error) {
	if err := binding.ValidName(name); err != nil {
		return MachineChannel{}, err
	}
	c, ok := machineChannel(name)
	if !ok {
		return MachineChannel{}, fmt.Errorf("the machine agent has no channel record for %s", name)
	}
	raw, err := json.Marshal(resetRequest{Reg: c.reg})
	if err != nil {
		return MachineChannel{}, err
	}
	if _, err := fsq.WriteFileAtomic(c.dir, name+".reset", append(raw, '\n'), 0o600); err != nil {
		return MachineChannel{}, err
	}
	return c, nil
}
