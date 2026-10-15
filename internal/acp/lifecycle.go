package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// pinLineStripRe matches a pin line with its line break, for stripping.
var pinLineStripRe = regexp.MustCompile(`(?m)^[ \t]*AMQ Remote session:[^\n]*(\n|$)`)

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
func sessionPin(systemPrompt, model string) (pin string, helper bool, err error) {
	fromPrompt, found := "", false
	for _, m := range pinLineRe.FindAllStringSubmatch(systemPrompt, -1) {
		if found && m[1] != fromPrompt {
			return "", false, conflictingPins("the system prompt", fromPrompt, "the system prompt", m[1])
		}
		fromPrompt, found = m[1], true
	}
	fromModel := ""
	if name, ok := strings.CutPrefix(model, bindingModelPrefix); ok {
		fromModel = name
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

// stripPinLines removes pin lines from prompt text. buzz-acp puts the system
// prompt into the first prompt for agents without system prompt support.
func stripPinLines(text string) string {
	if !strings.Contains(text, "AMQ Remote session:") {
		return text
	}
	return pinLineStripRe.ReplaceAllString(text, "")
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
		m, ok := s.readAgentMarker(bd.Name)
		if ok && m.DisplayName != "" {
			fmt.Fprintf(&b, "\n- %s: DM %q", bd.Name, m.DisplayName)
			continue
		}
		fmt.Fprintf(&b, "\n- %s: no agent yet; run `amq-acp setup --session %s` and import the file in Buzz Desktop", bd.Name, bd.Name)
	}
	return b.String()
}

// agentMarker is a pinned agent's record under <state>/agents/<pin>.json.
// It is rewritten on each tick, so its update time shows the agent runs.
type agentMarker struct {
	Name        string `json:"name"`
	PubKey      string `json:"pubkey"`
	DisplayName string `json:"display_name,omitempty"`
	PID         int    `json:"pid"`
	// Serves is the binding as last seen bound.
	Serves *binding.Binding `json:"serves,omitempty"`
	// Archived is the state the relay last accepted for this identity.
	Archived bool   `json:"archived"`
	Updated  string `json:"updated"`
}

func (s *Server) agentMarkerPath(name string) string {
	return filepath.Join(s.cfg.StateDir, "agents", name+".json")
}

func (s *Server) readAgentMarker(name string) (agentMarker, bool) {
	var m agentMarker
	raw, err := readSmallRegular(s.agentMarkerPath(name))
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return agentMarker{}, false
	}
	return m, true
}

// buzzCall runs one buzz CLI command as the agent. A variable so tests can
// record calls without a relay.
var buzzCall = func(budget time.Duration, args ...string) error {
	_, err := runBuzzCLI(budget, nil, args...)
	return err
}

// lifecycleTick keeps a pinned agent's Buzz identity in step with its
// binding. A missing binding archives the agent (kind 9035, signed by the
// agent itself, so no owner key is needed) and sets its status; a binding
// that is back unarchives it (kind 9036) and clears the status. The marker
// keeps the last state the relay accepted, so each change runs once; a
// failed call is retried on the next tick. An unreadable binding proves
// nothing and changes nothing.
func (s *Server) lifecycleTick(now time.Time) {
	if s.cfg.Pin == "" || s.cfg.PinErr != nil || buzzSelf == "" {
		return
	}
	m, _ := s.readAgentMarker(s.cfg.Pin)
	if m.PubKey != buzzSelf {
		// Another identity's marker: this agent was never archived by it.
		m = agentMarker{}
	}
	b, err := binding.ReadNamed(s.cfg.Pin)
	switch {
	case errors.Is(err, binding.ErrNone):
		if !m.Archived && buzzCall(s.cfg.PostTimeout, "agents", "archive", buzzSelf, "--reason", "retired") == nil {
			m.Archived = true
			_ = buzzCall(s.cfg.PostTimeout, "users", "set-status", "--text", "session ended")
		}
	case err == nil:
		if m.Archived && buzzCall(s.cfg.PostTimeout, "agents", "unarchive", buzzSelf, "--reason", "returned") == nil {
			m.Archived = false
			_ = buzzCall(s.cfg.PostTimeout, "users", "set-status", "--clear")
		}
		m.Serves = &b
	}
	m.Name, m.PubKey, m.DisplayName, m.PID = s.cfg.Pin, buzzSelf, buzzAgent.displayName, os.Getpid()
	m.Updated = now.UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.agentMarkerPath(s.cfg.Pin))
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_, _ = fsq.WriteFileAtomic(dir, s.cfg.Pin+".json", append(raw, '\n'), 0o600)
}
