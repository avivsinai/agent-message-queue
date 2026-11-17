package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

// The machine agent is one Buzz agent per machine that serves every
// session. The worker that leads its identity makes a private channel for
// each live named session, adds the owner as an admin and welcomes them
// with a mention. A prompt reaches a session only from that session's
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
// record. A create whose outcome is unknown, including one whose worker
// stopped before it wrote the result, is resolved by the channel id the
// record chose (lookupChosen). A record that exists but cannot be read stops
// all work on its name.

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
	// routeJudgeInterval is how often the relay-signed state of the active
	// routes is read.
	routeJudgeInterval = time.Minute
	// channelCreateTries bounds the refused creates of one record.
	channelCreateTries = 3
	// retiredKeep is how many retired records are kept before the prunable
	// ones are removed, the oldest first.
	retiredKeep = 256
)

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
	// Reg is made once per record. It keys the record's writes and is
	// written into the channel's description.
	Reg string `json:"reg"`
	// Chosen is the channel id a create asks for, written before any create
	// is signed, so the channel an uncertain create may have made is found
	// by its id.
	Chosen     string `json:"chosen_id,omitempty"`
	State      string `json:"state"`
	Archived   bool   `json:"archived"`
	OwnerAdded bool   `json:"owner_added"`
	Welcomed   bool   `json:"welcomed"`
	// Suspended is why the channel's relay-signed state denies this active
	// route; empty while it does not. A suspended route sends nothing and
	// takes no message. Only judgeRoutes sets it.
	Suspended string `json:"suspended,omitempty"`
	// Resumed is when the route last left suspension. The mirror posts no
	// turn that ended before it.
	Resumed string `json:"resumed,omitempty"`
	Creates int    `json:"creates"`
	// Pending is written before a create is sent and cleared with its
	// result, so a create whose worker stopped in between stays unresolved.
	Pending bool `json:"pending,omitempty"`
	// Doubted records that a create outcome was once unknown: no create is
	// ever sent again for this record.
	Doubted bool `json:"doubted,omitempty"`
	// ArchiveSent is written before the channel's archive is sent, so a
	// restore unarchives a channel that may be archived although no archive
	// was confirmed.
	ArchiveSent bool   `json:"archive_sent,omitempty"`
	RetiredBy   string `json:"retired_by,omitempty"`
	RetiredFrom string `json:"retired_from,omitempty"`
	// RetiredAt is when the record left its active slot, set once then;
	// empty in a record retired before it was recorded.
	RetiredAt string `json:"retired_at,omitempty"`
	SaidEnded bool   `json:"said_ended,omitempty"`
	LastError string `json:"last_error,omitempty"`
	LastTry   string `json:"last_try,omitempty"`
	Updated   string `json:"updated"`
}

// inDoubt reports whether a create of r may have made a channel that is not
// known yet.
func (r channelRecord) inDoubt() bool {
	return (r.Doubted || r.Pending) && r.ChannelID == ""
}

// prunable reports whether a retired record may be removed: the channel it
// made is archived, or it made none and no create is in doubt.
func (r channelRecord) prunable() bool {
	return r.Archived || (r.ChannelID == "" && !r.inDoubt())
}

func channelDirFor(stateDir, relayKey, machine string) string {
	return filepath.Join(stateDir, "channels", relayKey, machine)
}

func (s *Server) channelDir() string { return channelDirFor(s.cfg.StateDir, s.id.RelayKey, s.id.Agent) }

func (s *Server) retiredDir() string { return filepath.Join(s.channelDir(), "retired") }

// loadChannelRecord reads one record. found is false only when there is no
// file; a file that cannot be read or parsed is an error, never first use.
func loadChannelRecord(path string) (r channelRecord, found bool, err error) {
	c, err := recordCache.Load(path, func() (cachedRecord, error) {
		r, found, err := decodeChannelRecord(path)
		return cachedRecord{r, found}, err
	})
	return c.r, c.found || err != nil, err
}

type cachedRecord struct {
	r     channelRecord
	found bool
}

// recordCache keeps each decoded channel record while its file is
// unchanged: the machine worker reads every session's record on each tick.
var recordCache = fsq.NewVersionCache[cachedRecord](1024)

func decodeChannelRecord(path string) (r channelRecord, found bool, err error) {
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
	return s.writeState(s.recordPath(*r), append(raw, '\n'))
}

// writeState atomically replaces path, a file below the state directory.
// The directory above the state directory anchors the write, so no directory
// from the state directory down is followed as a link.
func (s *Server) writeState(path string, raw []byte) error {
	// The state dir is the operator's choice (AMQ_ACP_STATE_DIR may name a
	// link); everything AMQ creates below it is checked.
	anchor := s.cfg.StateDir
	rel, err := filepath.Rel(anchor, path)
	if err != nil {
		return err
	}
	_, err = fsq.WriteStateFile(anchor, rel, raw, 0o600)
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
	ctx := s.machineCtx()
	return ctx != nil && ctx.Err() != nil
}

// machineCtx is the machine worker's context, nil before its first tick.
func (s *Server) machineCtx() context.Context {
	if ctx := s.machine.ctx.Load(); ctx != nil {
		return *ctx
	}
	return nil
}

// lifeCtx is the machine worker's context, or the background context before
// its first tick.
func (s *Server) lifeCtx() context.Context {
	if ctx := s.machineCtx(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// retiredKey names an intention of the retired record r: what, r's reg, and
// when r left its slot in Unix seconds, 0 when RetiredAt is empty or
// unreadable.
func retiredKey(what string, r channelRecord) buzzop.Key {
	var at int64
	if t, err := time.Parse(time.RFC3339, r.RetiredAt); err == nil {
		at = t.Unix()
	}
	return buzzop.Key(what + "/" + r.Reg + "/" + strconv.FormatInt(at, 10))
}

// machineTick runs in the worker that leads the machine identity, until
// ctx ends. It writes the marker first, before any relay call, then applies
// reset requests, retires the records of bindings that are gone, provisions
// the live sessions, checks the hooks in their Claude settings files, and
// every lateReplyInterval works through the retired store. Its pace asks
// for the next tick in machineScanInterval while a record is set up or a
// reset waits, at a record's retry time, and else on a change to the
// bindings, the records or a watched settings file, or machineIdlePoll.
func (s *Server) machineTick(ctx context.Context, now time.Time) (p pace) {
	s.machine.ctx.Store(&ctx)
	// A daemon seen gone counts only on consecutive ticks that judge it,
	// and no daemon answer outlives the tick that asked it.
	gone := s.machine.goneDaemons
	s.machine.goneDaemons = nil
	s.appServers.Store(nil)
	if !s.cfg.Machine || s.id.Agent == "" || s.stopped() || !s.lead() {
		return pace{next: now.Add(machineScanInterval)}
	}
	if due(s.machine.marked, lateReplyInterval, now) {
		m := agentMarker{Name: machineName, DisplayName: s.id.DisplayName, RelayKey: s.id.RelayKey, Host: "standalone"}
		s.writeAgentMarker(s.agentMarkerDir(), m, now)
		s.machine.marked = now.UTC().Format(time.RFC3339)
	}
	if s.id.RelayKey == "" || s.id.Owner == "" {
		return pace{}
	}
	// With no relay connection no step can be sent or observed; the
	// records wait as they are.
	if s.offline() {
		return pace{next: now.Add(machineScanInterval)}
	}
	p.watch = s.sessionWatch()
	s.appServers.Store(s.judgeAppServers(newAppServerView(), gone))
	if s.applyResets(now) {
		p.due(now.Add(machineScanInterval))
	}
	s.retireGone(now)
	s.reattach(now)
	all, err := s.snapshot().list()
	if err == nil {
		for _, b := range all {
			if s.stopped() {
				return p
			}
			if s.provisionable(b) {
				s.provision(b, now)
			} else if s.awaitsThread(b) {
				p.due(now.Add(machineScanInterval))
			}
		}
	}
	if err == nil && !s.stopped() {
		s.watchHooks(all)
		for path := range s.machine.settings {
			p.watch = append(p.watch, path)
		}
	}
	if !s.stopped() && due(s.machine.swept, lateReplyInterval, now) {
		s.machine.swept = now.UTC().Format(time.RFC3339)
		s.sweepRetired(now)
	}
	if !s.stopped() && due(s.machine.judged, routeJudgeInterval, now) {
		s.machine.judged = now.UTC().Format(time.RFC3339)
		s.judgeRoutes(now)
	}
	for _, r := range s.snapshot().records {
		if at, ok := recordDue(r, now); ok {
			p.due(at)
		}
	}
	return p
}

// recordDue is when the slot record r needs its next step: at once while
// it is set up or retired, at its retry time while it waits on one (an
// uncertain create waits channelStepGap); ok is false for an active record
// and one that is never tried again.
func recordDue(r channelRecord, now time.Time) (at time.Time, ok bool) {
	switch {
	case r.Pending:
	case r.State == channelActive:
		return time.Time{}, false
	case r.State == channelFailed && (r.Doubted || r.Creates >= channelCreateTries):
		return time.Time{}, false
	case r.State == channelFailed, r.State == channelUncertain:
		gap := channelRetryGap
		if r.State == channelUncertain {
			gap = channelStepGap
		}
		if last, err := time.Parse(time.RFC3339, r.LastTry); err == nil {
			return last.Add(gap), true
		}
	}
	return now.Add(machineScanInterval), true
}

// awaitsThread reports whether b is a Codex app-server session that may
// become live with no change on disk: its thread is not judged loaded or
// ended yet, and no record of this agent serves it.
func (s *Server) awaitsThread(b binding.Binding) bool {
	if b.AppServer.Thread == "" || binding.ValidName(b.Name) != nil {
		return false
	}
	if state, _ := s.liveness(b); state != binding.Unknown {
		return false
	}
	if r, ok := s.snapshot().slot(b.Name); ok && r.State == channelActive {
		return false
	}
	return true
}

// machineState is the machine worker's own state: its context, pacing,
// and the settings files it watches (machine_hooks.go). The context is
// read from other goroutines (a steered command's wait), so it is atomic.
type machineState struct {
	ctx                            atomic.Pointer[context.Context]
	marked, swept, rebound, judged string
	settings                       map[string]settingsSeen
	// goneDaemons is each binding name whose recorded app-server daemon the
	// last tick saw gone (daemonGone), with that daemon.
	goneDaemons map[string]launch.HolderIdentity
}

// newAppServerView asks the Codex app-server daemons. It is a variable so
// tests dial none.
var newAppServerView = func() *codex.View {
	return codex.NewView(func(sock string) ([]string, error) { return codex.LoadedThreadsWithin(sock, 2*time.Second) }, codex.DaemonPID)
}

// appServerJudge is one machine tick's view of the Codex app-server
// threads: the daemons' answers, asked at most once per socket, and the
// bindings whose daemon was gone on this tick and the one before, each with
// that daemon.
type appServerJudge struct {
	view *codex.View
	gone map[string]launch.HolderIdentity
	// judged is each binding's liveness as this tick judged it, so the
	// workers ask the process table once per binding per tick. A judgement
	// older than machineScanInterval is made again; the app-server answers
	// of view are kept for the whole tick, as before.
	judged sync.Map // livenessKey -> judgement
}

// livenessKey is every binding field its liveness reads.
type livenessKey struct {
	root, target, carrier string
	holder                launch.HolderIdentity
	appServer             binding.AppServerThread
}

type judgement struct {
	state    binding.State
	evidence string
	at       time.Time
}

// judgeAppServers makes this tick's judge from view and the daemons the
// previous tick saw gone, and records the daemons this tick sees gone.
func (s *Server) judgeAppServers(view *codex.View, prev map[string]launch.HolderIdentity) *appServerJudge {
	j := &appServerJudge{view: view, gone: map[string]launch.HolderIdentity{}}
	seen := map[string]launch.HolderIdentity{}
	all, _ := s.snapshot().list()
	for _, b := range all {
		if !daemonGone(view, b) {
			continue
		}
		if d, ok := prev[b.Name]; ok && d == b.AppServer.Daemon {
			j.gone[b.Name] = d
		}
		seen[b.Name] = b.AppServer.Daemon
	}
	s.machine.goneDaemons = seen
	return j
}

// daemonGone reports whether b's thread is confirmed absent at its socket
// while the daemon it was bound in is proven not running. A failed probe is
// not absence, so it never counts. A Codex update restarts the daemon and the
// thread is loaded again in the new one, so this alone is not proof that the
// session ended.
func daemonGone(view *codex.View, b binding.Binding) bool {
	if b.AppServer.Thread == "" || b.AppServer.Daemon.PID <= 0 {
		return false
	}
	if !view.Absent(b) {
		return false
	}
	state, _ := launch.InspectHolder(b.AppServer.Daemon)
	return state == launch.LeaseStale
}

// liveness is b's liveness as the machine agent judges it. A Codex
// app-server thread is live while a daemon at its socket has it loaded. It
// has ended when the daemon it was bound in, still running, no longer has
// it loaded, or when that daemon was gone, with the thread loaded nowhere,
// on two ticks in a row. Any other binding's liveness is its holder's, as
// every binding's is before the first machine tick.
func (s *Server) liveness(b binding.Binding) (binding.State, string) {
	j := s.appServers.Load()
	if j == nil {
		return b.Liveness()
	}
	key := livenessKey{root: b.Root, target: b.Target, carrier: b.Carrier, holder: b.Holder, appServer: b.AppServer}
	now := time.Now()
	var state binding.State
	var evidence string
	if v, ok := j.judged.Load(key); ok && now.Sub(v.(judgement).at) < machineScanInterval {
		state, evidence = v.(judgement).state, v.(judgement).evidence
	} else {
		state, evidence = j.view.Liveness(b)
		j.judged.Store(key, judgement{state, evidence, now})
	}
	if d, ok := j.gone[b.Name]; ok && state == binding.Unknown && b.AppServer.Thread != "" && d == b.AppServer.Daemon {
		return binding.Ended, "the app-server process the thread was bound in is not running, and no app-server has it loaded"
	}
	return state, evidence
}

// provisionable reports whether the machine agent serves b: a named binding
// whose session is proven live.
func (s *Server) provisionable(b binding.Binding) bool {
	if b.Name == binding.LegacyName || binding.ValidName(b.Name) != nil {
		return false
	}
	state, _ := s.liveness(b)
	return state == binding.Live
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
		r = channelRecord{Relay: s.id.RelayURL, Machine: s.id.Agent, Name: b.Name, Identity: b.SessionIdentity(),
			Label: sessionAgentName(b), Reg: reg, State: channelCreating}
		if r.Chosen, err = newChannelID(); err != nil {
			return
		}
		if s.save(&r, now) != nil {
			return
		}
	}
	s.advance(&r, now)
}

// advance moves r one or more steps toward active: it resolves a create a
// stopped worker left pending, creates the channel, and sets it up.
func (s *Server) advance(r *channelRecord, now time.Time) {
	if r.Pending {
		// A create was sent and its result never written: the worker
		// stopped while it ran.
		r.State, r.Pending, r.Doubted, r.LastError = channelUncertain, false, true, "the outcome of a create was not recorded"
		_ = s.save(r, now)
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
		if !due(r.LastTry, channelStepGap, now) || !s.lookup(r, now) {
			return
		}
	default:
		return
	}
	if r.ChannelID == "" && !s.create(r, now) {
		return
	}
	s.finishSetup(r, now)
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
		s.retire(r, s.goneReason(r), now)
		return false
	case err != nil:
		return false
	case b.SessionIdentity() != r.Identity:
		s.retire(r, retiredReplaced, now)
		return false
	}
	if state, _ := s.liveness(b); state == binding.Ended {
		s.retire(r, retiredEnded, now)
		return false
	}
	return true
}

// judgeRoute judges an active route from its channel's relay-signed state.
// fault is the first required fact that f denies, in the owner's words;
// sound is true when f shows every required fact. A fact that f does not
// show is neither.
func judgeRoute(f buzzop.ChannelFacts, id buzzop.Identity) (fault string, sound bool) {
	switch {
	case f.Archived == buzzop.TriYes:
		return "the channel is archived", false
	case f.Private == buzzop.TriNo:
		return "the channel is not private", false
	case f.AdminsEvent != "" && f.Roles[id.Agent] != "owner":
		return "the machine agent is not the channel's owner", false
	case f.AdminsEvent != "" && !ownerSeated(f, id):
		return "the owner is not an admin of the channel", false
	}
	return "", routeReady(f, id)
}

// judgeRoutes reads the relay-signed state of each active record's channel.
// It suspends a route whose state denies a required fact and returns a
// suspended route whose state shows them all. A read that fails ends the
// pass: it proves nothing, and the relay is likely not answering.
func (s *Server) judgeRoutes(now time.Time) {
	for _, r := range s.activeRecords() {
		if s.stopped() {
			return
		}
		if r.State != channelActive || r.ChannelID == "" || r.Machine != s.id.Agent || r.Relay != s.id.RelayURL {
			continue
		}
		f, err := s.observe(r.ChannelID)
		if err != nil {
			return
		}
		fault, sound := judgeRoute(f, s.id)
		switch {
		case fault != "" && fault != r.Suspended:
			r.Suspended = fault
		case sound && r.Suspended != "":
			r.Suspended, r.Resumed = "", now.UTC().Format(time.RFC3339)
		default:
			continue
		}
		if s.unchanged(r) {
			_ = s.save(&r, now)
		}
	}
}

// create counts and sends one channels create. The count and the pending
// mark are written before the call, and the outcome after it, so a worker
// that stops in between leaves the create unresolved. Only an outcome that
// proves no channel was made (not sent, or rejected) fails the record;
// anything else may have made one, so the record is in doubt. It reports
// whether a channel id is known.
//
// The create asks for r.Chosen under the key create/<reg>, so every attempt
// of r sends one signed event. An attempt made with no relay connection
// sent nothing and leaves r to be created on a later pass without using up
// a try.
func (s *Server) create(r *channelRecord, now time.Time) bool {
	if r.Doubted || r.Creates >= channelCreateTries || !s.stillServes(*r, now) {
		return false
	}
	r.Creates++
	r.State, r.Pending, r.LastTry = channelCreating, true, now.UTC().Format(time.RFC3339)
	if s.save(r, now) != nil {
		return false
	}
	c := s.buzz.CreateChannel(s.lifeCtx(), s.createSpec(*r))
	r.Pending = false
	switch {
	case c.Reason == buzzop.ReasonNoConnection && (c.Outcome == buzzop.NotSent || c.Outcome == buzzop.Rejected):
		// Nothing was sent: the outcome is what an earlier attempt left,
		// and that attempt was counted when it was made.
		r.Creates--
		r.LastError = c.Reason
	case c.Outcome == buzzop.Accepted:
		r.ChannelID, r.LastError = c.ChannelID, ""
	case c.Outcome == buzzop.NotSent, c.Outcome == buzzop.Rejected:
		r.State, r.LastError = channelFailed, c.Reason
	default:
		r.State, r.Doubted, r.LastError = channelUncertain, true, c.Reason
	}
	if s.save(r, now) != nil {
		return false
	}
	return c.Outcome == buzzop.Accepted
}

// createSpec is the create of r: one key per registration, and the chosen
// id.
func (s *Server) createSpec(r channelRecord) buzzop.ChannelSpec {
	spec := buzzop.ChannelSpec{Key: buzzop.Key("create/" + r.Reg), Name: r.Label, About: "amq-remote " + r.Reg}
	spec.ID = r.Chosen
	return spec
}

// lookup looks for the channel an uncertain create of r may have made
// (lookupChosen). It reports whether r now has a channel to set up.
func (s *Server) lookup(r *channelRecord, now time.Time) bool {
	if !s.stillServes(*r, now) {
		return false
	}
	r.LastTry = now.UTC().Format(time.RFC3339)
	s.lookupChosen(r)
	return s.save(r, now) == nil && r.ChannelID != ""
}

// finishSetup adds the owner, welcomes them with a mention so the phone
// pushes, and activates the channel. Each step's outcome is written before
// the next; a failed step is retried after channelStepGap.
//
// The relay-signed state decides: the owner is added only while it does
// not show them as an admin or the owner, and the channel becomes active
// only once it shows a route (routeReady). A state that cannot be read, or
// does not show that yet, changes nothing and is read again after
// channelStepGap.
func (s *Server) finishSetup(r *channelRecord, now time.Time) {
	if r.State != channelCreating || r.ChannelID == "" {
		return
	}
	if r.LastError != "" && !due(r.LastTry, channelStepGap, now) {
		return
	}
	step := func(done *bool, write func() buzzop.Result) bool {
		if *done {
			return true
		}
		if !s.stillServes(*r, now) {
			return false
		}
		r.LastTry = now.UTC().Format(time.RFC3339)
		if res := write(); res.Outcome != buzzop.Accepted {
			r.LastError = res.Reason
			_ = s.save(r, now)
			return false
		}
		*done, r.LastError = true, ""
		return s.save(r, now) == nil
	}
	ctx := s.lifeCtx()
	if !step(&r.OwnerAdded, func() buzzop.Result {
		f, err := s.observe(r.ChannelID)
		switch {
		case err != nil:
			return buzzop.Result{Outcome: buzzop.NotSent, Reason: unreadableState + ": " + err.Error()}
		case ownerSeated(f, s.id):
			return buzzop.Result{Outcome: buzzop.Accepted}
		}
		return s.buzz.AddMember(ctx, buzzop.Key("member/"+r.Reg+"/"+s.id.Owner), r.ChannelID, s.id.Owner, "admin")
	}) {
		return
	}
	welcome := "AMQ Remote: this channel reaches " + r.Label + ". Write here to talk to that session."
	if !step(&r.Welcomed, func() buzzop.Result {
		return s.buzz.Send(ctx, buzzop.Message{Key: buzzop.Key("welcome/" + r.Reg), Channel: r.ChannelID,
			Content: welcome, Mention: s.id.Owner})
	}) {
		return
	}
	f, err := s.observe(r.ChannelID)
	if err != nil || !routeReady(f, s.id) {
		r.LastTry, r.LastError = now.UTC().Format(time.RFC3339), waitingForState
		if err != nil {
			r.LastError = unreadableState + ": " + err.Error()
		}
		_ = s.save(r, now)
		return
	}
	r.LastError = ""
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
		r.RetiredFrom, r.RetiredAt = r.State, now.UTC().Format(time.RFC3339)
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
	if err := s.writeState(slot, append(raw, '\n')); err != nil {
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
func (s *Server) goneReason(r channelRecord) string {
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
		if state, _ := s.liveness(t); state == binding.Ended {
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
	snap := s.snapshot()
	for _, r := range snap.activeRecords() {
		if s.stopped() {
			return
		}
		if r.State == channelRetired {
			s.finishRetire(r, now)
			continue
		}
		// The snapshot only lets a live binding be skipped: whether it is
		// gone or ended is decided from a fresh read, since a session can
		// attach again while an earlier record's relay call runs.
		if b, ok := snap.binding(r.Name); ok && s.live(b) {
			continue
		}
		b, err := binding.ReadNamed(r.Name)
		// A record under a name no binding can have serves nothing.
		if errors.Is(err, binding.ErrNone) || binding.ValidName(r.Name) != nil {
			s.retire(r, s.goneReason(r), now)
		} else if err == nil {
			if state, _ := s.liveness(b); state == binding.Ended {
				s.retire(r, retiredEnded, now)
			}
		}
	}
}

// activeRecords reads every good record in the name slots.
func (s *Server) activeRecords() []channelRecord {
	records, _ := s.readSlots()
	return records
}

// readSlots reads every good record in the name slots, also by slot name.
func (s *Server) readSlots() ([]channelRecord, map[string]channelRecord) {
	entries, _ := os.ReadDir(s.channelDir())
	var out []channelRecord
	slots := map[string]channelRecord{}
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && e.Type().IsRegular() {
			if r, ok := readChannelRecord(filepath.Join(s.channelDir(), e.Name())); ok {
				out = append(out, r)
				slots[name] = r
			}
		}
	}
	return out, slots
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
// active for a session that ended, and archives the channel the record
// made. archived is written only after the archive is confirmed; a failure
// is retried after channelRetryGap without limit. A retired record never
// gets add-member, a welcome or activation.
//
// The uncertain create is looked for by its chosen id, and the channel's
// relay-signed state is read before anything is sent: a channel it shows
// archived is recorded archived with no post and no archive, and a state
// that cannot be read, or that has no metadata event, sends nothing. The
// archive is keyed by the state it changes (stateKey).
func (s *Server) cleanRetired(r channelRecord, now time.Time) {
	if r.Archived || s.stopped() || (r.LastTry != "" && r.LastError != "" && !due(r.LastTry, channelRetryGap, now)) {
		return
	}
	if r.ChannelID == "" && !r.inDoubt() {
		return
	}
	if !s.unchanged(r) {
		return
	}
	r.LastTry = now.UTC().Format(time.RFC3339)
	fail := func(reason string) {
		r.LastError = reason
		_ = s.save(&r, now)
	}
	ctx := s.lifeCtx()
	if r.inDoubt() {
		id, ok := s.findChosen(r)
		if !ok {
			fail("could not look for the channel of an uncertain create")
			return
		}
		if id == "" {
			r.LastError = "no channel found yet for an uncertain create"
			_ = s.save(&r, now)
			return
		}
		r.ChannelID, r.Pending, r.Doubted = id, false, true
		if s.save(&r, now) != nil {
			return
		}
	}
	archiveKey := retiredKey("archive", r)
	if r.ChannelID != "" {
		f, err := s.observe(r.ChannelID)
		switch {
		case err != nil:
			fail(unreadableState + ": " + err.Error())
			return
		case f.Archived == buzzop.TriYes:
			r.Archived, r.LastError = true, ""
			_ = s.save(&r, now)
			return
		case f.MetaEvent == "":
			fail(waitingForState)
			return
		}
		archiveKey = stateKey("archive", r, f)
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
		_ = s.buzz.Send(ctx, buzzop.Message{Key: retiredKey("ended", r), Channel: r.ChannelID, Content: text})
	}
	if r.ChannelID != "" {
		if s.stopped() {
			return
		}
		r.ArchiveSent = true
		if s.save(&r, now) != nil {
			return
		}
		if res := s.buzz.SetArchived(ctx, archiveKey, r.ChannelID, true); res.Outcome != buzzop.Accepted {
			fail(res.Reason)
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
	if state, _ := s.liveness(cur); state == binding.Ended {
		return false
	}
	_, held, err := loadChannelRecord(s.slotPath(b.Name))
	return err == nil && !held
}

// restore brings back the channel of the same session attaching again: the
// newest retired record of b's name and identity that was active and ended
// with its session. It reports whether such a record exists, so no new
// channel is made while it is being brought back. Only an observation that
// the channel is not archived lets it become active (unarchived), and a
// record whose channel the owner archived (Archived without ArchiveSent) is
// never brought back: the session gets a new channel.
func (s *Server) restore(b binding.Binding, now time.Time) bool {
	var found *channelRecord
	for _, r := range s.retiredRecords() {
		if r.Archived && !r.ArchiveSent {
			continue
		}
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
	switch s.unarchived(&r, now) {
	case restoreWaits:
		return true
	case restoreOwnerArchived:
		return false
	}
	if !s.restorable(b, r) {
		return true
	}
	retiredPath := s.recordPath(r)
	r.State, r.RetiredBy, r.RetiredFrom, r.SaidEnded, r.LastError = channelActive, "", "", false, ""
	r.Suspended = ""
	if s.save(&r, now) != nil {
		return true
	}
	// A leftover retired copy (a stop before this removal is durable) is
	// dropped by sweepRetired: the active record of its reg supersedes it.
	_ = s.removeDurably(retiredPath)
	_ = s.buzz.Send(s.lifeCtx(), buzzop.Message{Key: retiredKey("returned", r), Channel: r.ChannelID,
		Content: "Connected again: " + r.Label + "."})
	return true
}

// restoreStep is what a restore may do next.
type restoreStep int

const (
	// restoreWaits: nothing proves the channel usable yet; r keeps its slot
	// in the retired store and is tried again after channelStepGap.
	restoreWaits restoreStep = iota
	// restoreReady: the relay shows the channel not archived.
	restoreReady
	// restoreOwnerArchived: the relay shows the channel archived by someone
	// other than AMQ Remote; it is never unarchived or brought back.
	restoreOwnerArchived
)

// unarchived reads the relay-signed state of the channel of the retired
// record r and says whether a restore may make it active. An archive is
// AMQ Remote's exactly when ArchiveSent is set (it is saved before the
// archive is sent); only such an archive is undone, under a key of the
// state it changes (stateKey). An accepted unarchive clears both flags and
// is never taken as proof: the state is read again at once, and only a
// read that shows the channel not archived makes it ready. Any other
// archive is the owner's: r is recorded archived and is not brought back.
func (s *Server) unarchived(r *channelRecord, now time.Time) restoreStep {
	if r.LastError != "" && !due(r.LastTry, channelStepGap, now) {
		return restoreWaits
	}
	r.LastTry = now.UTC().Format(time.RFC3339)
	f, err := s.observe(r.ChannelID)
	switch {
	case err != nil:
		r.LastError = unreadableState + ": " + err.Error()
	case f.Archived == buzzop.TriNo:
		r.Archived, r.ArchiveSent, r.LastError = false, false, ""
		return restoreReady
	case f.Archived == buzzop.TriYes && !r.ArchiveSent:
		r.Archived, r.LastError = true, ""
		_ = s.save(r, now)
		return restoreOwnerArchived
	case f.Archived == buzzop.TriYes && f.MetaEvent != "":
		res := s.buzz.SetArchived(s.lifeCtx(), stateKey("unarchive", *r, f), r.ChannelID, false)
		if res.Outcome != buzzop.Accepted {
			r.LastError = res.Reason
			break
		}
		r.Archived, r.ArchiveSent, r.LastError = false, false, waitingForState
		if s.save(r, now) != nil {
			return restoreWaits
		}
		if f, err := s.observe(r.ChannelID); err == nil && f.Archived == buzzop.TriNo {
			r.LastError = ""
			return restoreReady
		}
	default:
		r.LastError = waitingForState
	}
	_ = s.save(r, now)
	return restoreWaits
}

// stateKey names the archive or unarchive (what) of r that changes the
// relay-signed state f: the observed metadata event. A new state gives a
// new key, so an archive after an unarchive is a new event; the same state
// gives the same key, so a repeat sends nothing new.
func stateKey(what string, r channelRecord, f buzzop.ChannelFacts) buzzop.Key {
	return buzzop.Key(what + "/" + r.Reg + "/" + f.MetaEvent)
}

// resetRequest asks the leader to retire the record with Reg and make a
// new one. It is written by amq-remote channel reset beside the record.
type resetRequest struct {
	Reg string `json:"reg"`
}

// applyResets retires each record named by a reset request whose reg still
// matches it, and reports whether a request stays. The next provisioning makes a new record with a new reg; the
// retired one keeps being reconciled and archived, so a late channel of an
// uncertain create is still found and archived. A request is dropped once it is
// applied or names no current record; one whose request or record cannot be
// read, or whose retirement failed, stays for the next scan.
func (s *Server) applyResets(now time.Time) (left bool) {
	matches, _ := filepath.Glob(filepath.Join(s.channelDir(), "*.reset"))
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), ".reset")
		var req resetRequest
		raw, err := readSmallRegular(path)
		if err != nil || json.Unmarshal(raw, &req) != nil {
			left = true
			continue
		}
		r, found, err := loadChannelRecord(s.slotPath(name))
		switch {
		case err != nil:
			left = true
			continue
		case found && r.Reg == req.Reg && r.State != channelRetired:
			if !s.retire(r, retiredReset, now) {
				left = true
				continue
			}
		}
		if os.Remove(path) != nil {
			left = true
		}
	}
	return left
}

// channelRoute returns the active record of channel under this agent's own
// relay and key directory. A record anywhere else is never read for routing.
func (s *Server) channelRoute(channel string) (channelRecord, bool) {
	if channel == "" || s.id.RelayKey == "" || s.id.Agent == "" {
		return channelRecord{}, false
	}
	for _, r := range s.activeRecords() {
		if r.ChannelID == channel && routable(r, s.id) {
			return r, true
		}
	}
	return channelRecord{}, false
}

// errChannelNotConnected refuses a prompt from a channel whose record names
// a session identity that the binding no longer has.
var errChannelNotConnected = errors.New("this channel's session is not connected")

// machineBinding is the binding a machine agent's session drives: the
// session named by the active record of the channel the session's first
// turn came from. The current turn must carry that same channel. Anything
// that does not resolve gets the directory.
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
		return binding.Binding{}, &unpinnedError{}
	}
	r, ok := s.channelRoute(route)
	if !ok {
		return binding.Binding{}, &unpinnedError{}
	}
	return routeBinding(r)
}

// routeBinding is the binding of the session the active record r was made
// for; errChannelNotConnected when that session is no longer bound.
func routeBinding(r channelRecord) (binding.Binding, error) {
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
		if state, _ := s.liveness(bd); state != binding.Live || bd.Name == binding.LegacyName {
			continue
		}
		if listed == 0 {
			b.WriteString(" Sessions connected on this machine:")
		}
		listed++
		label := sessionAgentName(bd)
		r, ok := records[bd.Name]
		switch {
		case ok && r.State == channelActive && r.Suspended != "" && r.Identity == bd.SessionIdentity():
			fmt.Fprintf(&b, "\n- %s: its channel %q is suspended (%s)", label, r.Label, r.Suspended)
		case ok && r.State == channelActive && r.Identity == bd.SessionIdentity():
			fmt.Fprintf(&b, "\n- %s: write in the %q channel", label, r.Label)
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
	// State is creating, uncertain, active, failed, retired or suspended.
	// A suspended active channel's relay-signed state denies the route;
	// Error is why.
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

// BindingStateDir is the machine agent's state directory, beside the
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
	if r.State == channelActive && r.Suspended != "" {
		c.State, c.Error = "suspended", r.Suspended
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
	if _, err := fsq.WriteStateFile(filepath.Dir(c.dir), filepath.Join(filepath.Base(c.dir), name+".reset"), append(raw, '\n'), 0o600); err != nil {
		return MachineChannel{}, err
	}
	return c, nil
}
