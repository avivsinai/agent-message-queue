package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// One Buzz agent serves one session. Its pin names that session's binding:
// the line "AMQ Remote session: <name>" in the agent's system prompt (new
// agents; a Desktop draft and an import can carry a system prompt but not a
// model), or the model amq-remote:<name> (agents made before the line). Buzz
// Desktop passes both to this process as BUZZ_ACP_SYSTEM_PROMPT and
// BUZZ_ACP_MODEL, so the pin never depends on Buzz calling session/set_model.

const (
	envBuzzSystemPrompt = "BUZZ_ACP_SYSTEM_PROMPT"
	envBuzzModel        = "BUZZ_ACP_MODEL"
	envBuzzDisplayName  = "BUZZ_ACP_DISPLAY_NAME"
)

// pinLineRe matches one pin line and captures the name it gives.
var pinLineRe = regexp.MustCompile(`(?m)^[ \t]*AMQ Remote session:[ \t]*(\S*)[ \t]*\r?$`)

// helperLineRe matches the system prompt line that makes an agent the
// per-machine helper. The helper serves no session and never delivers,
// whatever is bound.
var helperLineRe = regexp.MustCompile(`(?m)^[ \t]*AMQ Remote helper[ \t]*\r?$`)

// buzzAgent is the managed agent's record as Desktop passed it, captured
// before PrepareWorkerEnv strips BUZZ_*.
var buzzAgent struct{ systemPrompt, model, displayName string }

func captureBuzzAgent() {
	buzzAgent.systemPrompt = os.Getenv(envBuzzSystemPrompt)
	buzzAgent.model = strings.TrimSpace(os.Getenv(envBuzzModel))
	buzzAgent.displayName = strings.TrimSpace(os.Getenv(envBuzzDisplayName))
}

// sessionPin resolves the binding an agent serves, or the helper role.
// Either pin source may name the binding; both naming the same binding is
// accepted. Two different names, a helper line beside a session pin, or a
// name that is not a binding name, is an error that refuses every prompt.
// A pin line or helper line that does not parse fails closed rather than
// leaving the agent unpinned, where it would drive the only binding.
func sessionPin(systemPrompt, model string) (pin string, helper bool, err error) {
	lower := strings.ToLower(systemPrompt)
	lines := pinLineRe.FindAllStringSubmatch(systemPrompt, -1)
	if strings.Count(lower, "amq remote session:") != len(lines) {
		return "", false, errors.New(`invalid session pin: write the system prompt line as "AMQ Remote session: <name>" on its own line`)
	}
	if strings.Count(lower, "amq remote helper") != len(helperLineRe.FindAllString(systemPrompt, -1)) {
		return "", false, errors.New(`invalid helper line: write it as "AMQ Remote helper" on its own line`)
	}
	fromPrompt, found := "", false
	for _, m := range lines {
		if found && m[1] != fromPrompt {
			return "", false, conflictingPins("the system prompt", fromPrompt, "the system prompt", m[1])
		}
		fromPrompt, found = m[1], true
	}
	fromModel, modelPin := strings.CutPrefix(model, bindingModelPrefix)
	switch {
	case !modelPin:
		fromModel = ""
	case fromModel == "":
		return "", false, fmt.Errorf("invalid session pin: model %q names no binding", model)
	}
	if found && fromModel != "" && fromModel != fromPrompt {
		return "", false, conflictingPins("the system prompt", fromPrompt, "the model", fromModel)
	}
	pin = fromModel
	if found {
		pin = fromPrompt
	}
	helper = helperLineRe.MatchString(systemPrompt)
	switch {
	case helper && (found || pin != ""):
		return "", false, fmt.Errorf("conflicting session pins: the system prompt makes this agent the helper and also names session %q; the helper serves no session", pin)
	case helper || (!found && pin == ""):
		return "", helper, nil
	}
	if err := binding.ValidName(pin); err != nil {
		return "", false, fmt.Errorf("invalid session pin %q: %v", pin, err)
	}
	return pin, false, nil
}

func conflictingPins(source, name, otherSource, other string) error {
	return fmt.Errorf("conflicting session pins: %s names %q and %s names %q; make them name one session in Buzz Desktop", source, name, otherSource, other)
}

// unpinnedError is the helper, or an unpinned agent with several bindings.
// Neither chooses a session, so it lists the bound ones instead.
type unpinnedError struct {
	bindings []binding.Binding
	helper   bool
}

func (e *unpinnedError) Error() string {
	return "this agent serves no session; DM the agent of the session you want"
}

// unpinnedReply lists each bound session and the agent to DM for it.
func (s *Server) unpinnedReply(e *unpinnedError) string {
	var b strings.Builder
	if e.helper {
		b.WriteString("This is the AMQ Remote helper; it does not run prompts. Sessions bound on this machine:")
	} else {
		b.WriteString("Several sessions are bound and this agent names none, so it does not run prompts. Sessions bound on this machine:")
	}
	for _, bd := range e.bindings {
		var names []string
		for _, m := range s.agentMarkers(bd.Name) {
			if m.DisplayName != "" && m.running(time.Now()) {
				names = append(names, strconv.Quote(m.DisplayName))
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(&b, "\n- %s: DM %s", bd.Name, strings.Join(names, " or "))
			continue
		}
		fmt.Fprintf(&b, "\n- %s: %s", bd.Name, s.noAgentText(bd, e.helper))
	}
	return b.String()
}

// noAgentText says what to do for a session with no running agent. Only
// the helper drafts, and only for a live session with tries left.
func (s *Server) noAgentText(b binding.Binding, helper bool) string {
	importStep := fmt.Sprintf("run `amq-acp setup --session %s` and import the file in Buzz Desktop", b.Name)
	if markers := s.agentMarkers(b.Name); len(markers) > 0 {
		for _, m := range markers {
			if m.running(time.Now()) {
				return "its agent runs; DM it in Buzz"
			}
		}
		return "its agent is not running; start it in Buzz Desktop"
	}
	if !helper || b.Name == binding.LegacyName {
		return "no agent yet; " + importStep
	}
	if state, _ := b.Liveness(); state != binding.Live {
		return fmt.Sprintf("session state %s, so no agent is drafted; run /amq-remote in that session, or %s", state, importStep)
	}
	if s.helperRequest(b).Count >= helperRequestTries {
		return "Buzz Desktop did not create its agent; run /amq-remote in that session again, or " + importStep
	}
	return "no agent yet; Buzz Desktop opens \"Create agent\" for it"
}

// agentMarker is a pinned agent's record under
// <state>/agents/<pin>/<pubkey>.json. Each identity has its own file, so two
// agents pinned to one session never reset each other's state. It is
// rewritten on each tick, so its update time shows the agent runs.
type agentMarker struct {
	Name        string `json:"name"`
	PubKey      string `json:"pubkey"`
	DisplayName string `json:"display_name,omitempty"`
	PID         int    `json:"pid"`
	// Serves is the binding as last seen bound.
	Serves *binding.Binding `json:"serves,omitempty"`
	// Archived is the state the relay last accepted for this identity.
	Archived bool `json:"archived"`
	// Channel is where the agent posts on its own: a session agent's latest
	// DM, or the helper's control channel. OwnerAdmin records that the
	// owner is an admin of the control channel. A marker from before it
	// has only owner_added (a plain member), so setup runs again.
	Channel    string `json:"channel,omitempty"`
	OwnerAdmin bool   `json:"owner_admin,omitempty"`
	// The rest are the helper's: when its control channel setup last
	// failed, control channels it gave up on, and when and for which
	// session it last sent a draft.
	SetupFailed  string   `json:"setup_failed,omitempty"`
	Dropped      []string `json:"dropped_channels,omitempty"`
	LastDraft    string   `json:"last_draft,omitempty"`
	LastDraftFor string   `json:"last_draft_for,omitempty"`
	Updated      string   `json:"updated"`
}

// agentMarkerFresh is how old a marker may be while its agent still runs.
// A running agent rewrites it on every tick, but a tick waits behind a
// sweep whose posts can take minutes on a slow relay.
const agentMarkerFresh = 10 * time.Minute

// running reports whether the marker's agent rewrote it recently. A stopped
// or deleted agent stops rewriting it, so it leaves the session list.
func (m agentMarker) running(now time.Time) bool {
	updated, err := time.Parse(time.RFC3339, m.Updated)
	return err == nil && now.Sub(updated) <= agentMarkerFresh
}

func (s *Server) agentMarkerDir(pin string) string {
	return filepath.Join(s.cfg.StateDir, "agents", pin)
}

func readAgentMarker(path string) (agentMarker, bool) {
	var m agentMarker
	raw, err := readSmallRegular(path)
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return agentMarker{}, false
	}
	return m, true
}

// agentMarkers returns the record of every identity pinned to pin.
func (s *Server) agentMarkers(pin string) []agentMarker {
	entries, _ := os.ReadDir(s.agentMarkerDir(pin))
	var out []agentMarker
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if m, ok := readAgentMarker(filepath.Join(s.agentMarkerDir(pin), e.Name())); ok {
			out = append(out, m)
		}
	}
	return out
}

// buzzCall runs one buzz CLI command as the agent and returns its output. A
// variable so tests can record calls without a relay.
var buzzCall = func(budget time.Duration, args ...string) ([]byte, error) {
	return runBuzzCLI(budget, nil, args...)
}

// buzzRun is buzzCall when only the outcome matters.
func buzzRun(budget time.Duration, args ...string) error {
	_, err := buzzCall(budget, args...)
	return err
}

// lifecycleTick keeps a pinned agent's Buzz identity in step with its
// binding. A missing binding archives the agent (kind 9035, signed by the
// agent itself, so no owner key is needed) and sets its status; a binding
// that is back unarchives it (kind 9036) and clears the status. The marker
// keeps the last state the relay accepted, so each change runs once; a
// failed call is retried on the next tick. An unreadable binding proves
// nothing and changes nothing.
//
// When it unarchives, it posts "Connected again" to its latest DM.
//
// Buzz may run N identical workers of one agent; only the worker that leads
// its identity runs the tick, so each change runs once, not N times.
func (s *Server) lifecycleTick(now time.Time) {
	if buzzSelf == "" || (!s.cfg.Helper && (s.cfg.Pin == "" || s.cfg.PinErr != nil)) {
		return
	}
	if s.cfg.Helper {
		if s.lead(helperName) {
			s.helperTick(now)
		}
		return
	}
	if !s.lead(s.cfg.Pin) {
		return
	}
	dir := s.agentMarkerDir(s.cfg.Pin)
	m, _ := readAgentMarker(filepath.Join(dir, buzzSelf+".json"))
	if raw, err := readSmallRegular(filepath.Join(dir, buzzSelf+".dm")); err == nil && strings.TrimSpace(string(raw)) != "" {
		m.Channel = strings.TrimSpace(string(raw))
	} else {
		s.mu.Lock()
		if s.lastChannel != "" {
			m.Channel = s.lastChannel
		}
		s.mu.Unlock()
	}
	b, err := binding.ReadNamed(s.cfg.Pin)
	switch {
	case errors.Is(err, binding.ErrNone):
		if !m.Archived && buzzRun(s.cfg.PostTimeout, "agents", "archive", buzzSelf, "--reason", "retired") == nil {
			m.Archived = true
			_ = buzzRun(s.cfg.PostTimeout, "users", "set-status", "--text", "session ended")
		}
	case err == nil:
		if m.Archived && buzzRun(s.cfg.PostTimeout, "agents", "unarchive", buzzSelf, "--reason", "returned") == nil {
			m.Archived = false
			_ = buzzRun(s.cfg.PostTimeout, "users", "set-status", "--clear")
			if m.Channel != "" {
				_, _ = postAnswer(m.Channel, "Connected again: "+sessionAgentName(b), s.cfg.PostTimeout)
			}
		}
		m.Serves = &b
	}
	m.Name, m.DisplayName = s.cfg.Pin, buzzAgent.displayName
	s.writeAgentMarker(dir, m, now)
}

// lead reports whether this worker leads the lifecycle of its Buzz identity
// under pin. The first worker to take <pin>/<self>.lock leads for the life of
// Serve; when it exits, the kernel drops the lock and the next tick of
// another worker takes it. Without advisory locks amq-acp runs one worker
// (PrepareWorkerEnv), which leads. A lock error skips this tick.
func (s *Server) lead(pin string) bool {
	if s.leader != nil || !lock.AdvisoryLockAvailable() {
		return true
	}
	dir := s.agentMarkerDir(pin)
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	release, ok, err := lock.TryExclusive(filepath.Join(dir, buzzSelf+".lock"))
	if err != nil || !ok {
		return false
	}
	s.leader = release
	return true
}

// resign releases the lifecycle lead, if this worker holds it.
func (s *Server) resign() {
	if s.leader != nil {
		s.leader()
		s.leader = nil
	}
}

// recordDM keeps channel as the latest DM of a pinned agent across its
// workers, so the worker that leads its lifecycle posts "Connected again"
// there. Every DM prompt writes it: the last writer is the latest DM.
func (s *Server) recordDM(channel string) {
	if !s.cfg.RemoteBinding || s.cfg.Helper || s.cfg.Pin == "" || s.cfg.PinErr != nil || buzzSelf == "" {
		return
	}
	dir := s.agentMarkerDir(s.cfg.Pin)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_, _ = fsq.WriteFileAtomic(dir, buzzSelf+".dm", []byte(channel+"\n"), 0o600)
}

// writeAgentMarker stamps m as this process's and writes it under dir.
func (s *Server) writeAgentMarker(dir string, m agentMarker, now time.Time) {
	m.PubKey, m.PID = buzzSelf, os.Getpid()
	m.Updated = now.UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_, _ = fsq.WriteFileAtomic(dir, buzzSelf+".json", append(raw, '\n'), 0o600)
}

// The per-machine helper ("AMQ Remote helper" in its system prompt) creates
// the session agents. It never delivers. On each tick it:
//   - creates the private control channel "amq-remote @ <machine>" once and
//     adds the owner as an admin: Desktop accepts an agent draft only from a
//     channel that both the sending agent and the owner belong to, and adds
//     each new agent to that channel as the owner, which a relay may allow
//     only an owner or admin of a private channel to do;
//   - sends one draft-create per helperRequestGap for a live binding that
//     no agent has a marker for yet (the fewest tries, then the oldest), so
//     Desktop opens "Create agent" with the session's name and pin filled
//     in. A binding whose session is unknown or ended, and the legacy
//     unnamed binding, never get one. A binding gets at most
//     helperRequestTries drafts, and a re-attach (a newer bound_at) starts a
//     new budget; the draft is ephemeral, so a closed Desktop drops it.

// helperName is the helper's marker directory under agents/. It is not a
// valid binding name, so it never meets a session's.
const helperName = "_helper"

const (
	helperRequestTries = 3
	helperRequestGap   = 10 * time.Minute
)

// helperRequest is the bookkeeping of the draft requests for one binding,
// under <state>/requests/<name>.json.
type helperRequest struct {
	Count int    `json:"count"`
	Last  string `json:"last"`
}

func (s *Server) helperTick(now time.Time) {
	if buzzSelf == "" {
		return
	}
	dir := s.agentMarkerDir(helperName)
	m, _ := readAgentMarker(filepath.Join(dir, buzzSelf+".json"))
	m.Name, m.DisplayName = helperName, buzzAgent.displayName
	defer func() { s.writeAgentMarker(dir, m, now) }()
	if buzzOwner == "" || !s.helperChannel(&m, now) {
		return
	}
	// Desktop shows one draft at a time and drops a draft that arrives
	// while one is open, so the helper sends at most one draft per window
	// across all sessions. The window ends early once the drafted session's
	// agent runs: the owner closed that dialog.
	if last, err := time.Parse(time.RFC3339, m.LastDraft); err == nil && now.Sub(last) < helperRequestGap &&
		(m.LastDraftFor == "" || len(s.agentMarkers(m.LastDraftFor)) == 0) {
		return
	}
	all, err := binding.List()
	if err != nil {
		return
	}
	var next *binding.Binding
	var nextReq helperRequest
	for i, b := range all {
		if !s.draftable(b) {
			continue
		}
		req := s.helperRequest(b)
		if req.Count >= helperRequestTries {
			continue
		}
		if next == nil || req.Count < nextReq.Count || (req.Count == nextReq.Count && req.Last < nextReq.Last) {
			next, nextReq = &all[i], req
		}
	}
	if next == nil {
		return
	}
	sent, err := s.requestSessionAgent(m.Channel, *next, nextReq, now)
	if sent {
		m.LastDraft, m.LastDraftFor = now.UTC().Format(time.RFC3339), next.Name
	}
	if err != nil && s.channelGone(m.Channel, err.Error()) {
		// Set a new control channel up after the backoff.
		dropChannel(&m, now)
	}
}

// helperDroppedMax bounds the control channels the helper remembers giving
// up on.
const helperDroppedMax = 8

// dropChannel gives up on the helper's control channel: the next setup,
// after the backoff, adopts or creates another one.
func dropChannel(m *agentMarker, now time.Time) {
	if m.Channel != "" {
		m.Dropped = append(m.Dropped, m.Channel)
		if len(m.Dropped) > helperDroppedMax {
			m.Dropped = m.Dropped[len(m.Dropped)-helperDroppedMax:]
		}
	}
	m.Channel, m.OwnerAdmin = "", false
	m.SetupFailed = now.UTC().Format(time.RFC3339)
}

// helperChannel makes sure the control channel exists and the owner is an
// admin of it. Before it creates one, it adopts a private channel of the same name
// that the helper owns, unless it gave up on that channel. After a failure
// it waits helperRequestGap, so a failing relay never gets a request every
// tick and an unclear create never leaves a stream of new channels. A
// channel that refuses the owner is given up.
func (s *Server) helperChannel(m *agentMarker, now time.Time) bool {
	if m.Channel != "" && m.OwnerAdmin {
		return true
	}
	if failed, err := time.Parse(time.RFC3339, m.SetupFailed); err == nil && now.Sub(failed) < helperRequestGap {
		return false
	}
	m.SetupFailed = now.UTC().Format(time.RFC3339)
	name := "amq-remote"
	if machine := binding.Machine(); machine != "" {
		name += " @ " + machine
	}
	if m.Channel == "" {
		out, err := buzzCall(s.cfg.PostTimeout, "channels", "list", "--member", "--visibility", "private")
		var listed []struct {
			ChannelID string `json:"channel_id"`
			Name      string `json:"name"`
		}
		if err != nil || json.Unmarshal(out, &listed) != nil {
			return false
		}
		for _, c := range listed {
			if c.Name == name && c.ChannelID != "" && !slices.Contains(m.Dropped, c.ChannelID) && s.ownsChannel(c.ChannelID) {
				m.Channel = c.ChannelID
				break
			}
		}
	}
	if m.Channel == "" {
		out, err := buzzCall(s.cfg.PostTimeout, "channels", "create", "--name", name, "--type", "stream", "--visibility", "private")
		var created struct {
			Accepted  bool   `json:"accepted"`
			ChannelID string `json:"channel_id"`
		}
		if err != nil || json.Unmarshal(out, &created) != nil || !created.Accepted || created.ChannelID == "" {
			return false
		}
		m.Channel = created.ChannelID
	}
	// Adding the owner again with a new role changes the role, so a channel
	// set up before the owner was made an admin is upgraded here.
	out, err := buzzCall(s.cfg.PostTimeout, "channels", "add-member", "--channel", m.Channel, "--pubkey", buzzOwner, "--role", "admin")
	var added struct {
		Accepted bool `json:"accepted"`
	}
	if err != nil || json.Unmarshal(out, &added) != nil || !added.Accepted {
		failure := string(out)
		if err != nil {
			failure = err.Error()
		}
		if s.channelGone(m.Channel, failure) {
			dropChannel(m, now)
		}
		return false
	}
	m.OwnerAdmin, m.SetupFailed = true, ""
	return true
}

// ownsChannel reports whether the helper is the owner of channel, so it
// never adopts a channel someone else made with the same name.
func (s *Server) ownsChannel(channel string) bool {
	owner, ok := s.channelOwner(channel)
	return ok && owner
}

// channelOwner reports whether the helper owns channel; ok is false when
// the members could not be read, which proves nothing.
func (s *Server) channelOwner(channel string) (owner, ok bool) {
	out, err := buzzCall(s.cfg.PostTimeout, "channels", "members", "--channel", channel)
	var members []struct {
		PubKey string `json:"pubkey"`
		Role   string `json:"role"`
	}
	if err != nil || json.Unmarshal(out, &members) != nil {
		return false, false
	}
	for _, mb := range members {
		if mb.PubKey == buzzSelf && mb.Role == "owner" {
			return true, true
		}
	}
	return false, true
}

// channelGone reports whether a refused call shows the control channel is
// unusable: the refusal says it is archived, or the relay lists the helper
// no longer as its owner. A failure that proves neither, such as a timeout,
// keeps the channel.
func (s *Server) channelGone(channel, failure string) bool {
	if channel == "" {
		return false
	}
	if strings.Contains(strings.ToLower(failure), "archived") {
		return true
	}
	owner, ok := s.channelOwner(channel)
	return ok && !owner
}

// draftable reports whether the helper may draft an agent for b: a named
// binding whose session is proven live and that no agent has a marker for.
func (s *Server) draftable(b binding.Binding) bool {
	if b.Name == binding.LegacyName || len(s.agentMarkers(b.Name)) > 0 {
		return false
	}
	state, _ := b.Liveness()
	return state == binding.Live
}

// helperRequest returns b's draft bookkeeping. A re-attach (a bound_at newer
// than the last request) is a new request with a full budget.
func (s *Server) helperRequest(b binding.Binding) helperRequest {
	var req helperRequest
	if raw, err := readSmallRegular(filepath.Join(s.cfg.StateDir, "requests", b.Name+".json")); err == nil {
		_ = json.Unmarshal(raw, &req)
	}
	last, err := time.Parse(time.RFC3339, req.Last)
	if bound, berr := time.Parse(time.RFC3339, b.BoundAt); err == nil && berr == nil && bound.After(last) {
		return helperRequest{}
	}
	return req
}

// requestSessionAgent records one more attempt for b and sends its
// draft-create. It reports whether the attempt was recorded and sent, and
// the draft-create error.
func (s *Server) requestSessionAgent(channel string, b binding.Binding, req helperRequest, now time.Time) (bool, error) {
	dir := filepath.Join(s.cfg.StateDir, "requests")
	req.Count++
	req.Last = now.UTC().Format(time.RFC3339)
	raw, err := json.Marshal(req)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return false, nil
	}
	// The attempt is recorded first, so a failing Desktop or relay never
	// gets more than helperRequestTries requests.
	if _, err := fsq.WriteFileAtomic(dir, b.Name+".json", append(raw, '\n'), 0o600); err != nil {
		return false, nil
	}
	return true, buzzRun(s.cfg.PostTimeout, "agents", "draft-create", "--channel", channel, "--display-name", sessionAgentName(b), "--system-prompt", "AMQ Remote session: "+b.Name)
}

// sessionAgentName is the Buzz agent name for b's session: its label, else
// "AMQ: <name>" for a binding written before labels.
func sessionAgentName(b binding.Binding) string {
	if b.Label != "" {
		return b.Label
	}
	return "AMQ: " + b.Name
}
