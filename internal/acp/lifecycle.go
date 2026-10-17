package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
		if e.helper {
			fmt.Fprintf(&b, "\n- %s: no agent yet; Buzz Desktop opens \"Create agent\" for it", bd.Name)
			continue
		}
		fmt.Fprintf(&b, "\n- %s: no agent yet; run `amq-acp setup --session %s` and import the file in Buzz Desktop", bd.Name, bd.Name)
	}
	return b.String()
}

// agentMarker is a pinned agent's record under
// <state>/agents/<pin>/<pubkey>.json. Each identity has its own file, so two
// agents pinned to one session never reset each other's state (review of S3
// P1-1). It is rewritten on each tick, so its update time shows the agent
// runs.
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
	// DM, or the helper's control channel. OwnerAdded records that the
	// owner is a member of the control channel.
	Channel    string `json:"channel,omitempty"`
	OwnerAdded bool   `json:"owner_added,omitempty"`
	Updated    string `json:"updated"`
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
func (s *Server) lifecycleTick(now time.Time) {
	if s.cfg.Helper {
		s.helperTick(now)
		return
	}
	if s.cfg.Pin == "" || s.cfg.PinErr != nil || buzzSelf == "" {
		return
	}
	dir := s.agentMarkerDir(s.cfg.Pin)
	m, _ := readAgentMarker(filepath.Join(dir, buzzSelf+".json"))
	s.mu.Lock()
	if s.lastChannel != "" {
		m.Channel = s.lastChannel
	}
	s.mu.Unlock()
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
//     adds the owner: Desktop accepts an agent draft only from a channel
//     that both the sending agent and the owner belong to, and adds each new
//     agent to that channel;
//   - for each live binding that no agent has a marker for yet, sends one
//     draft-create, so Desktop opens "Create agent" with the session's name
//     and pin filled in. A binding whose session is unknown or ended, and
//     the legacy unnamed binding, never get one. A request is retried at
//     most helperRequestTries times, helperRequestGap apart, and a re-attach
//     (a newer bound_at) starts a new budget; the draft is ephemeral, so a
//     closed Desktop drops it.

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
	if buzzSelf == "" || buzzOwner == "" {
		return
	}
	dir := s.agentMarkerDir(helperName)
	m, _ := readAgentMarker(filepath.Join(dir, buzzSelf+".json"))
	m.Name, m.DisplayName = helperName, buzzAgent.displayName
	defer func() { s.writeAgentMarker(dir, m, now) }()
	if m.Channel == "" {
		name := "amq-remote"
		if machine := binding.Machine(); machine != "" {
			name += " @ " + machine
		}
		out, err := buzzCall(s.cfg.PostTimeout, "channels", "create", "--name", name, "--type", "stream", "--visibility", "private")
		var created struct {
			Accepted  bool   `json:"accepted"`
			ChannelID string `json:"channel_id"`
		}
		if err != nil || json.Unmarshal(out, &created) != nil || !created.Accepted || created.ChannelID == "" {
			return
		}
		m.Channel = created.ChannelID
	}
	if !m.OwnerAdded {
		if buzzRun(s.cfg.PostTimeout, "channels", "add-member", "--channel", m.Channel, "--pubkey", buzzOwner) != nil {
			return
		}
		m.OwnerAdded = true
	}
	all, err := binding.List()
	if err != nil {
		return
	}
	for _, b := range all {
		if b.Name == binding.LegacyName || len(s.agentMarkers(b.Name)) > 0 {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Live {
			s.requestSessionAgent(m.Channel, b, now)
		}
	}
}

// requestSessionAgent sends one draft-create for b's agent unless the
// request budget for b is spent or the last request is too recent.
func (s *Server) requestSessionAgent(channel string, b binding.Binding, now time.Time) {
	dir := filepath.Join(s.cfg.StateDir, "requests")
	var req helperRequest
	if raw, err := readSmallRegular(filepath.Join(dir, b.Name+".json")); err == nil {
		_ = json.Unmarshal(raw, &req)
	}
	last, err := time.Parse(time.RFC3339, req.Last)
	if bound, berr := time.Parse(time.RFC3339, b.BoundAt); err == nil && berr == nil && bound.After(last) {
		req = helperRequest{} // a re-attach is a new request
	}
	if req.Count >= helperRequestTries || (req.Count > 0 && err == nil && now.Sub(last) < helperRequestGap) {
		return
	}
	req.Count++
	req.Last = now.UTC().Format(time.RFC3339)
	raw, err := json.Marshal(req)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	// The attempt is recorded first, so a failing Desktop or relay never
	// gets more than helperRequestTries requests.
	if _, err := fsq.WriteFileAtomic(dir, b.Name+".json", append(raw, '\n'), 0o600); err != nil {
		return
	}
	_ = buzzRun(s.cfg.PostTimeout, "agents", "draft-create", "--channel", channel, "--display-name", sessionAgentName(b), "--system-prompt", "AMQ Remote session: "+b.Name)
}

// sessionAgentName is the Buzz agent name for b's session: its label, else
// "AMQ: <name>" for a binding written before labels.
func sessionAgentName(b binding.Binding) string {
	if b.Label != "" {
		return b.Label
	}
	return "AMQ: " + b.Name
}
