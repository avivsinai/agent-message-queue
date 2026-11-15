package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzdirect"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
)

// Every serve runs a machineSet. It runs the machine agent enrolled under
// <amq home>/remote/machine (machinecred) when exactly one is, its grant
// holds, the relay names its self key, and no other machine agent runs
// here. The serve whose StartMachine takes the agent's machine lock runs
// it; the others wait and take over when that serve stops. The set owns
// the agent's relay connection and the replay of posts whose outcome is
// unknown; the engine (acp) owns everything else.

const (
	machineStatusFile = "status.json"
	// machineStatusEvery is the shortest gap between two status writes.
	machineStatusEvery = time.Second
	// machineSelfEvery is how often the relay's NIP-11 self key is read.
	machineSelfEvery = time.Minute
	// machineReplayEvery is how often posts of unknown outcome are sent
	// again while connected, besides once on each connect.
	machineReplayEvery = 30 * time.Second
	// machineReplayTimeout bounds one replayed post.
	machineReplayTimeout = 30 * time.Second
)

// Refusal codes in the machine agent's status file.
const (
	codeIdentityAmbiguous = "identity_ambiguous"
	codeNoAuthority       = "no_authority"
	codeAuthorityExpired  = "authority_expired"
	codeRelaySelfUnknown  = "relay_self_unknown"
	codeRelaySelfChanged  = "relay_self_changed"
	codeAuthRefused       = "auth_refused"
	codeOtherMachineAgent = "other_machine_agent"
)

// machineStatus is <amq home>/remote/machine/<agent>/status.json: what the
// machine agent does in this serve. It is a snapshot, not authority.
type machineStatus struct {
	// State is serving, waiting or refused.
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
	// Line says why the agent does not serve, and the user's next step.
	Line          string `json:"line,omitempty"`
	RelayURL      string `json:"relay_url,omitempty"`
	RelayState    string `json:"relay_state,omitempty"`
	RelayError    string `json:"relay_error,omitempty"`
	GrantNotAfter string `json:"grant_not_after,omitempty"`
	PID           int    `json:"pid"`
	Updated       string `json:"updated,omitempty"`
}

func (st machineStatus) refused(code, line string) machineStatus {
	st.State, st.Code, st.Line = "refused", code, line
	return st
}

func (st machineStatus) waiting(line string) machineStatus {
	st.State, st.Code, st.Line = "waiting", "", line
	return st
}

// selfAnswer is one read of a relay's NIP-11 self key.
type selfAnswer struct {
	url, self string
	err       error
}

// machineSet follows the enrolled machine credential while serve runs. One
// goroutine, serve's tick, changes it; mu serializes that with close.
type machineSet struct {
	home   string // the AMQ home; "" when it cannot be resolved
	ctx    context.Context
	stderr io.Writer

	mu     sync.Mutex
	closed bool
	run    *machineRun

	// The self key of the relay selfURL: the first answer, which stays for
	// the life of the process while the relay does. A later different answer
	// sets selfMoved, which stops the agent until a restart. A failed read
	// proves nothing.
	selfURL, self string
	selfErr       error
	selfMoved     bool
	selfAt        time.Time // when the last read began
	selfBusy      bool
	selfDone      chan selfAnswer

	said      map[string]string // what each report key last said
	written   string            // the status last written, without its time
	writtenAt time.Time
}

func newMachineSet(ctx context.Context, stderr io.Writer) *machineSet {
	home, err := amqhome.Dir()
	if err != nil {
		say(stderr, "machine agent: no AMQ home: %v", err)
		home = ""
	}
	return &machineSet{home: home, ctx: ctx, stderr: stderr, selfDone: make(chan selfAnswer, 1), said: map[string]string{}}
}

// tick reconciles the machine agent with the enrolled credential and the
// markers, and writes its status.
func (ms *machineSet) tick(now time.Time) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.closed || ms.home == "" {
		return
	}
	st, agents, ok := ms.reconcile(now)
	if ok {
		ms.show(st, agents, now)
	}
}

// close stops the agent: its workers, then its relay client.
func (ms *machineSet) close() {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.closed = true
	ms.drop()
}

// reconcile starts, keeps or stops the agent and returns its status and the
// agents whose status file shows it. ok is false when the credentials could
// not be listed; everything then stays as it is.
func (ms *machineSet) reconcile(now time.Time) (st machineStatus, agents []string, ok bool) {
	ms.takeSelf()
	agents, err := machinecred.Agents(ms.home)
	if err != nil {
		ms.sayOnce("agents", fmt.Sprintf("machine agent: the enrolled credentials do not list, so it stays as it is: %v", err))
		return st, nil, false
	}
	delete(ms.said, "agents")
	if ms.run != nil && ms.run.started {
		if st, ok := ms.keep(agents, now); ok {
			return st, []string{ms.run.id.Agent}, true
		}
		ms.drop()
	}
	switch len(agents) {
	case 0:
		ms.drop()
		return st, nil, true
	case 1:
	default:
		ms.drop()
		return st.refused(codeIdentityAmbiguous, fmt.Sprintf("%d machine agent keys under %s; remove the directory of the one you do not use.",
			len(agents), filepath.Join(ms.home, "remote", "machine"))), agents, true
	}
	agent := agents[0]
	cred, err := machinecred.Load(ms.home, agent)
	if err != nil {
		ms.drop()
		return st.refused(codeNoAuthority, noAuthorityLine(agent, err)), agents, true
	}
	url := buzzop.NormalizeRelay(cred.Relay)
	st.RelayURL, st.GrantNotAfter = url, grantEnd(cred)
	if line := grantEnded(agent, cred, now); line != "" {
		ms.drop()
		return st.refused(codeAuthorityExpired, line), agents, true
	}
	if other := otherMachineAgent(agent, now); other != "" {
		ms.drop()
		return st.refused(codeOtherMachineAgent, otherMachineLine(other)), agents, true
	}
	ms.readSelf(url, now)
	switch {
	case ms.selfMoved:
		ms.drop()
		return st.refused(codeRelaySelfChanged, url+" now publishes a different self key; restart amq-remote serve to adopt it."), agents, true
	case ms.self == "" && ms.selfErr != nil:
		ms.drop()
		return st.refused(codeRelaySelfUnknown, url+" publishes no self key, so channel state cannot be verified; nothing is created until it does."), agents, true
	case ms.self == "":
		return st.waiting(""), agents, true
	}
	if r := ms.run; r != nil && (r.id.Agent != agent || r.id.RelayURL != url || r.id.Owner != cred.Tag.OwnerPubKey || r.id.RelaySelf != ms.self) {
		ms.drop()
	}
	if ms.run == nil {
		run, err := newMachineRun(ms.home, agent, cred, ms.self)
		if err != nil {
			line := fmt.Sprintf("%s: %v", short(agent), err)
			ms.sayOnce("start", "machine agent: "+line)
			return st.waiting(line), agents, true
		}
		ms.run = run
	}
	started, err := ms.run.start(cred)
	if err != nil {
		line := fmt.Sprintf("%s: %v", short(agent), err)
		ms.sayOnce("start", "machine agent: "+line)
		return st.waiting(line), agents, true
	}
	delete(ms.said, "start")
	if !started {
		return st.waiting(anotherServeLine(agent, now)), agents, true
	}
	if st, ok := ms.keep(agents, now); ok {
		return st, agents, true
	}
	ms.drop()
	return st.waiting(""), agents, true
}

// keep reports the running agent's status, or ok false when it must stop:
// its credential is gone, another machine agent runs here, or the relay's
// self key changed. A credential changed in any other way keeps the agent
// as it started; a restart adopts the change.
func (ms *machineSet) keep(agents []string, now time.Time) (st machineStatus, ok bool) {
	run := ms.run
	agent := run.id.Agent
	if !slices.Contains(agents, agent) {
		return st, false
	}
	cred, err := machinecred.Load(ms.home, agent)
	if err != nil || otherMachineAgent(agent, now) != "" {
		return st, false
	}
	ms.readSelf(run.id.RelayURL, now)
	if ms.selfMoved {
		return st, false
	}
	if len(agents) != 1 || buzzop.NormalizeRelay(cred.Relay) != run.id.RelayURL || cred.Tag.OwnerPubKey != run.id.Owner {
		ms.sayOnce("adopt", fmt.Sprintf("machine agent %s: the enrolled machine credentials changed; it keeps running as it started, and a restart of amq-remote serve adopts the change.", short(agent)))
	} else {
		delete(ms.said, "adopt")
	}
	cs := run.client.Status()
	st = machineStatus{State: "serving", RelayURL: run.id.RelayURL, RelayState: string(cs.State), RelayError: cs.Err, GrantNotAfter: grantEnd(cred)}
	switch {
	case grantEnded(agent, cred, now) != "":
		st = st.refused(codeAuthorityExpired, grantEnded(agent, cred, now))
	case cs.Err == relay.ErrAuthRefused.Error():
		st = st.refused(codeAuthRefused, fmt.Sprintf("%s refused the machine agent's key %s; the owner must allow it on %s.", run.id.RelayURL, short(agent), run.id.RelayURL))
	}
	return st, true
}

// drop stops and forgets the agent.
func (ms *machineSet) drop() {
	if ms.run != nil {
		ms.run.stop()
		ms.run = nil
	}
}

// readSelf starts a read of url's self key when none runs and the last one
// began machineSelfEvery ago. A new relay forgets what the old one said.
func (ms *machineSet) readSelf(url string, now time.Time) {
	if url != ms.selfURL {
		ms.selfURL, ms.self, ms.selfErr, ms.selfMoved, ms.selfAt = url, "", nil, false, time.Time{}
	}
	if ms.selfBusy || !ms.selfAt.IsZero() && now.Sub(ms.selfAt) < machineSelfEvery {
		return
	}
	ms.selfBusy, ms.selfAt = true, now
	ctx, done := ms.ctx, ms.selfDone
	go func() {
		self, err := buzzio.RelaySelf(ctx, url)
		done <- selfAnswer{url: url, self: self, err: err}
	}()
}

// takeSelf applies a finished read of the self key.
func (ms *machineSet) takeSelf() {
	select {
	case a := <-ms.selfDone:
		ms.selfBusy = false
		switch {
		case a.url != ms.selfURL:
		case a.err != nil:
			if ms.self == "" {
				ms.selfErr = a.err
			}
		case ms.self == "":
			ms.self, ms.selfErr = a.self, nil
		case a.self != ms.self:
			ms.selfMoved = true
		}
	default:
	}
}

// show logs a change of what the agent does, once, and writes its status
// file to each agent's directory at most once a machineStatusEvery and only
// when it changed. With no agent it writes nothing.
func (ms *machineSet) show(st machineStatus, agents []string, now time.Time) {
	msg := st.Line
	if st.State == "serving" && msg == "" {
		msg = fmt.Sprintf("%s serves on %s", short(agents[0]), st.RelayURL)
	}
	if msg != "" {
		ms.sayOnce("state", "machine agent: "+msg)
	} else {
		delete(ms.said, "state")
	}
	if len(agents) == 0 {
		return
	}
	st.PID = os.Getpid()
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	key := strings.Join(agents, ",") + "\n" + string(raw)
	if key == ms.written || now.Sub(ms.writtenAt) < machineStatusEvery {
		return
	}
	st.Updated = now.UTC().Format(time.RFC3339)
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	for _, agent := range agents {
		if _, err := fsq.WriteStateFile(ms.home, filepath.Join("remote", "machine", agent, machineStatusFile), append(out, '\n'), 0o600); err != nil {
			ms.sayOnce("status", fmt.Sprintf("machine agent status: %v", err))
			return
		}
	}
	delete(ms.said, "status")
	ms.written, ms.writtenAt = key, now
}

// sayOnce reports msg under key unless the key last reported the same.
func (ms *machineSet) sayOnce(key, msg string) {
	if ms.said[key] == msg {
		return
	}
	ms.said[key] = msg
	say(ms.stderr, "%s", msg)
}

// machineRun is one machine agent: its server, built when the set first
// tries to start it, and, once StartMachine took the machine lock, its
// outbox, port and relay client.
type machineRun struct {
	home string
	id   buzzop.Identity
	ops  string // the outbox directory
	srv  *acp.Server
	port portHolder
	conn atomic.Pointer[relay.Conn]

	started     bool
	stopWorkers func()
	out         *outbox.Outbox
	client      *relay.Client
	cancel      context.CancelFunc
	done        chan struct{}

	// A replay pass holds replayMu; stop sets stopping and takes it, so no
	// pass writes the outbox after the machine lock is released.
	replayMu sync.Mutex
	stopping atomic.Bool
}

// newMachineRun builds the agent's server. Its state is the binding mode
// state directory, as the engine's; the outbox is <state>/ops/<relay
// key>/<agent>.
func newMachineRun(home, agent string, cred machinecred.Credential, self string) (*machineRun, error) {
	path, err := binding.Path()
	if err != nil {
		return nil, err
	}
	id := buzzop.Identity{
		RelayURL: buzzop.NormalizeRelay(cred.Relay), RelayKey: buzzop.RelayKey(cred.Relay), RelaySelf: self,
		Owner: cred.Tag.OwnerPubKey, Agent: agent, DisplayName: defaultMachineName(),
	}
	run := &machineRun{home: home, id: id, ops: filepath.Join(filepath.Dir(path), "acp", "ops", id.RelayKey, agent), done: make(chan struct{})}
	run.srv = acp.NewStandalone(acp.Standalone{Identity: id, Transport: &run.port, Online: run.online}, version)
	return run, nil
}

// start takes the machine lock and, holding it, opens the outbox, installs
// the port and starts the relay client. started is false, with no error,
// while another process holds the lock.
func (run *machineRun) start(cred machinecred.Credential) (started bool, err error) {
	if run.started {
		return true, nil
	}
	stop, ok := run.srv.StartMachine()
	if !ok {
		return false, nil
	}
	out, err := outbox.Open(run.ops)
	var port *buzzdirect.Port
	if err == nil {
		port, err = buzzdirect.New(buzzdirect.Config{Identity: run.id, Secret: cred.Key.Secret(), Conn: run.live, Outbox: out})
	}
	if err != nil {
		stop()
		return false, err
	}
	run.port.p.Store(port)
	run.started, run.stopWorkers, run.out = true, stop, out
	run.client = relay.NewClient(run.config)
	run.client.OnConnect = run.onConnect
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go func() {
		defer close(run.done)
		_ = run.client.Run(ctx)
	}()
	return true, nil
}

// stop ends the workers, which releases the machine lock, then the relay
// client. A run that never started has nothing to stop.
func (run *machineRun) stop() {
	if !run.started {
		return
	}
	run.stopping.Store(true)
	run.replayMu.Lock()
	run.stopWorkers()
	run.replayMu.Unlock()
	run.cancel()
	<-run.done
}

// online reports whether the port is installed and a relay connection is up.
func (run *machineRun) online() bool {
	return run.port.p.Load() != nil && run.conn.Load() != nil
}

// live is the port's connection getter.
func (run *machineRun) live() buzzdirect.Conn {
	if c := run.conn.Load(); c != nil {
		return c
	}
	return nil
}

// config is the relay client's ConfigFunc: the credential as enrolled now.
// A renewed grant for the same key, relay and owner is adopted on the next
// connection; another relay or owner is refused until serve restarts, since
// the outbox and identity are the ones the agent started with. The secret
// goes only into the config.
func (run *machineRun) config() (relay.Config, error) {
	cred, err := machinecred.Load(run.home, run.id.Agent)
	if err != nil {
		return relay.Config{}, err
	}
	if buzzop.NormalizeRelay(cred.Relay) != run.id.RelayURL || cred.Tag.OwnerPubKey != run.id.Owner {
		return relay.Config{}, errors.New("the enrolled grant names another relay or owner; restart amq-remote serve to adopt it")
	}
	end, err := cred.Tag.NotAfter()
	if err != nil {
		return relay.Config{}, err
	}
	if err := cred.Tag.Window(time.Now().Unix()); err != nil {
		return relay.Config{}, fmt.Errorf("%w: %v", relay.ErrGrantExpired, err)
	}
	return relay.Config{
		URL:      cred.Relay,
		Secret:   cred.Key.Secret(),
		AuthTag:  []string{"auth", cred.Tag.OwnerPubKey, cred.Tag.Conditions, cred.Tag.SigHex()},
		NotAfter: time.Unix(end, 0),
	}, nil
}

// onConnect serves one authenticated connection: the port and the engine
// use it until it ends, and posts of unknown outcome are sent again at once
// and every machineReplayEvery.
func (run *machineRun) onConnect(ctx context.Context, conn *relay.Conn) {
	run.conn.Store(conn)
	defer run.conn.CompareAndSwap(conn, nil)
	t := time.NewTicker(machineReplayEvery)
	defer t.Stop()
	for {
		run.replay(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// replay sends again, with the same event, each channel message (kind 9)
// and edit (kind 40003) whose last outcome is unknown and whose next attempt
// is due. A not_sent record is the engine's to retry, and the engine
// resolves its own channel creates, member adds and archives.
func (run *machineRun) replay(ctx context.Context) {
	run.replayMu.Lock()
	defer run.replayMu.Unlock()
	port := run.port.p.Load()
	if port == nil || run.stopping.Load() {
		return
	}
	recs, err := run.out.Unsettled()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, r := range recs {
		if run.stopping.Load() || ctx.Err() != nil {
			return
		}
		if r.Kind != 9 && r.Kind != 40003 || r.Outcome != buzzop.Unknown || r.NextAttempt > now {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, machineReplayTimeout)
		port.Replay(rctx, buzzop.Key(r.Key))
		cancel()
	}
}

// portHolder is the agent's transport: its port once the machine lock is
// held and the outbox open, and until then a transport with no relay
// connection. The port writes the outbox even with no connection, and the
// outbox has one writer, the lock holder, so no port exists before the
// lock is held.
type portHolder struct {
	p atomic.Pointer[buzzdirect.Port]
}

var (
	noPortResult = buzzop.Result{Outcome: buzzop.NotSent, Reason: buzzop.ReasonNoConnection}
	errNoPort    = errors.New(buzzop.ReasonNoConnection)
)

func (h *portHolder) CreateChannel(ctx context.Context, spec buzzop.ChannelSpec) buzzop.Created {
	if p := h.p.Load(); p != nil {
		return p.CreateChannel(ctx, spec)
	}
	return buzzop.Created{Result: noPortResult}
}

func (h *portHolder) AddMember(ctx context.Context, key buzzop.Key, channel, pubkey, role string) buzzop.Result {
	if p := h.p.Load(); p != nil {
		return p.AddMember(ctx, key, channel, pubkey, role)
	}
	return noPortResult
}

func (h *portHolder) SetArchived(ctx context.Context, key buzzop.Key, channel string, archived bool) buzzop.Result {
	if p := h.p.Load(); p != nil {
		return p.SetArchived(ctx, key, channel, archived)
	}
	return noPortResult
}

func (h *portHolder) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	if p := h.p.Load(); p != nil {
		return p.Send(ctx, m)
	}
	return noPortResult
}

func (h *portHolder) Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result {
	if p := h.p.Load(); p != nil {
		return p.Edit(ctx, key, channel, eventID, content)
	}
	return noPortResult
}

func (h *portHolder) Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error) {
	if p := h.p.Load(); p != nil {
		return p.Reactions(ctx, eventID)
	}
	return nil, errNoPort
}

func (h *portHolder) ChannelFacts(ctx context.Context, channel string) (buzzop.ChannelFacts, error) {
	if p := h.p.Load(); p != nil {
		return p.ChannelFacts(ctx, channel)
	}
	return buzzop.ChannelFacts{}, errNoPort
}

func (h *portHolder) Replay(ctx context.Context, key buzzop.Key) buzzop.Result {
	if p := h.p.Load(); p != nil {
		return p.Replay(ctx, key)
	}
	return noPortResult
}

func (h *portHolder) Lookup(ctx context.Context, eventID string) (bool, error) {
	if p := h.p.Load(); p != nil {
		return p.Lookup(ctx, eventID)
	}
	return false, errNoPort
}

// machineMarker is what serve reads of one machine agent marker
// (agents/_machine/<pubkey>.json).
type machineMarker struct {
	key, display string
	worker       launch.HolderIdentity
	state        string
}

// machineMarkers reads every machine agent marker as it reads at now.
func machineMarkers(now time.Time) []machineMarker {
	path, err := binding.Path()
	if err != nil {
		return nil
	}
	dir := filepath.Join(filepath.Dir(path), "acp", "agents", machineMarkerDir)
	entries, _ := os.ReadDir(dir)
	var out []machineMarker
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(dir, e.Name()), maxMarkerBytes)
		var m struct {
			DisplayName string                `json:"display_name"`
			Updated     string                `json:"updated"`
			Worker      launch.HolderIdentity `json:"worker"`
			Parent      launch.HolderIdentity `json:"parent"`
		}
		if err != nil || json.Unmarshal(raw, &m) != nil {
			continue
		}
		out = append(out, machineMarker{key: key, display: m.DisplayName, worker: m.Worker, state: acp.AgentState(m.Updated, m.Worker, m.Parent, now)})
	}
	return out
}

// otherMachineAgent names a machine agent of another key whose marker
// reads running or idle, or returns "". A marker this process wrote is an
// agent this serve ran and stopped, not another one.
func otherMachineAgent(agent string, now time.Time) string {
	for _, m := range machineMarkers(now) {
		if m.key == agent || m.worker.PID == os.Getpid() {
			continue
		}
		if m.state == agentRunning || m.state == agentIdle {
			return nonEmpty(m.display, short(m.key))
		}
	}
	return ""
}

// anotherServeLine says that another serve runs the agent, naming its
// process when its marker does.
func anotherServeLine(agent string, now time.Time) string {
	for _, m := range machineMarkers(now) {
		if m.key == agent && m.state == agentRunning && m.worker.PID > 0 && m.worker.PID != os.Getpid() {
			return fmt.Sprintf("%s runs in another amq-remote serve (pid %d); this serve takes over when that one stops.", short(agent), m.worker.PID)
		}
	}
	return short(agent) + " runs in another amq-remote serve; this serve takes over when that one stops."
}

func otherMachineLine(name string) string {
	return fmt.Sprintf("Buzz Desktop's machine agent %s runs on this Mac; stop it in Buzz Desktop (Agents, %s, Stop).", name, name)
}

func noAuthorityLine(agent string, err error) string {
	if errors.Is(err, machinecred.ErrNoAuthority) {
		return short(agent) + ": the owner has not signed its grant; run amq-owner-sign --machine <request>, then amq-remote machine enroll --grant <file>."
	}
	return fmt.Sprintf("%s: its enrolled grant does not load (%v); run amq-owner-sign --machine <request>, then amq-remote machine enroll --grant <file>.", short(agent), err)
}

// grantEnded is the refusal of a grant whose window does not hold at now,
// or "".
func grantEnded(agent string, cred machinecred.Credential, now time.Time) string {
	if cred.Tag.Window(now.Unix()) == nil {
		return ""
	}
	return fmt.Sprintf("%s: the owner's grant ended %s; run amq-remote machine request and have the owner sign again.", short(agent), grantEnd(cred))
}

// grantEnd is the grant's end in RFC 3339, or "".
func grantEnd(cred machinecred.Credential) string {
	end, err := cred.Tag.NotAfter()
	if err != nil {
		return ""
	}
	return time.Unix(end, 0).UTC().Format(time.RFC3339)
}

// short is the first 8 characters of a public key.
func short(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}
