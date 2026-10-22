// Package host runs one Agent Client Protocol (ACP) agent as an AMQ
// participant: amq-acp host. The host is the agent's only ACP client. It
// consumes its own inbox one message at a time, runs each message as one
// session/prompt turn, and replies on the message's thread with refs naming
// it. It never restarts the agent and never grants a tool call.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

// extensionLayer names the host's agent-scope extension directory:
// <root>/agents/<me>/extensions/acp-host/.
const extensionLayer = "acp-host"

// Label marks every reply the host writes. A message that carries it is
// another host's reply and never runs a turn.
const Label = "acp-host"

// protocolVersion is the ACP major version the host speaks to its agent.
const protocolVersion = 1

const (
	scanInterval       = time.Second
	handshakeTimeout   = 60 * time.Second
	defaultTurnTimeout = 30 * time.Minute
	cancelGrace        = 30 * time.Second
	// bodyLimit leaves room for the reply header under the message size cap.
	bodyLimit = format.MaxMessageSize - 64*1024
)

// AutonomyNotice is the one sentence the startup line and the README carry.
const AutonomyNotice = "Every tool call the agent asks to run is rejected unless the agent's own arguments grant autonomy, for example Grok's --always-approve."

// ActionRequiredError is a refusal the operator must resolve before the
// host can run: a second host for the handle, an agent that needs a login,
// an unsupported protocol version, or a platform without file locks.
type ActionRequiredError struct{ Message string }

func (e *ActionRequiredError) Error() string { return e.Message }

func actionRequired(format string, args ...any) error {
	return &ActionRequiredError{Message: fmt.Sprintf(format, args...)}
}

// Config is the identity a host binds at start. It is fixed for the life of
// the process.
type Config struct {
	Root    string
	Me      string
	Cwd     string
	Version string
	// Agent names the agent executable in log lines.
	Agent string
	Log   io.Writer
	// AgentExit, when set, reports how the agent process exited, for the
	// reply to a turn the agent did not survive. It may wait briefly.
	AgentExit func() string
}

// Host is one running amq-acp host.
type Host struct {
	cfg      Config
	identity fsq.DeliveryRootIdentity
	release  func()

	conn      *conn
	sessionID string
	// resumed is true when the session was loaded from session.json.
	resumed   bool
	agentName string
	timeout   time.Duration
	// unreadable remembers inbox files the host leaves in place, so each
	// is logged once.
	unreadable map[string]bool

	mu   sync.Mutex
	turn *turnCollect

	// generation names the opened ACP session on the bridge seam; handled
	// holds the bridge requests already decided. Both belong to the loop.
	generation string
	handled    map[string]bool
	// eventMu serializes appends to the bridge event streams.
	eventMu sync.Mutex
}

// turnCollect gathers one turn's answer. The connection's reader fills it;
// the turn reads it after the prompt response, which the reader delivers
// only after every earlier session/update.
type turnCollect struct {
	sessionID string
	text      strings.Builder
	refused   int
	// cancelled is closed when the host cancels the turn, so an approval
	// waiting for the owner answers cancelled; done is closed when the turn
	// ends, so no approval outlives it.
	cancelled chan struct{}
	done      chan struct{}
	// bridge is the remote request this turn serves; nil for a mailbox turn.
	bridge *bridgeTurn
}

// Open authenticates the root, takes the host lock for the handle, and
// returns a host ready to run. Close releases the lock.
func Open(cfg Config) (*Host, error) {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if !lock.AdvisoryLockAvailable() {
		return nil, actionRequired("amq-acp host needs advisory file locks, which this platform does not provide")
	}
	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := acp.VerifySessionPinOn(root); err != nil {
		return nil, err
	}
	if err := root.EnsureAgentDirs(cfg.Me); err != nil {
		return nil, err
	}
	extDir := filepath.Join("agents", cfg.Me, "extensions", extensionLayer)
	// The lock file is created through the authenticated root; the lock
	// itself is held on its path for the life of the process.
	lockFile, err := root.OpenLockFile(extDir, "host.lock", 0o600)
	if err != nil {
		return nil, err
	}
	_ = lockFile.Close()
	release, ok, err := lock.TryExclusive(filepath.Join(cfg.Root, extDir, "host.lock"))
	if err != nil {
		return nil, err
	}
	if !ok {
		pid, _ := root.ReadFile(filepath.Join(extDir, "host.pid"))
		return nil, actionRequired("another amq-acp host already serves %s (pid %s)", cfg.Me, strings.TrimSpace(string(pid)))
	}
	if _, err := root.WriteFileAtomic(extDir, "host.pid", []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		release()
		return nil, err
	}
	return &Host{cfg: cfg, identity: identity, release: release, timeout: defaultTurnTimeout, unreadable: map[string]bool{}}, nil
}

// Close releases the host lock.
func (h *Host) Close() {
	if h.release != nil {
		h.release()
		h.release = nil
	}
}

// Run speaks ACP to the agent on in and out until ctx ends or the agent
// exits. It returns nil only when ctx ended. An agent that exits is an
// error; the host never restarts it.
func (h *Host) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	h.conn = newConn(in, out, h.onRequest, h.onNotify)
	if err := h.handshake(ctx); err != nil {
		return err
	}
	opened := "new"
	if h.resumed {
		opened = "resumed"
	}
	_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %s in %s answers AMQ messages through %s (ACP session %s, %s). %s\n", h.cfg.Me, h.cfg.Root, h.cfg.Agent, h.sessionID, opened, AutonomyNotice)
	stopBridge, err := h.startBridge()
	if err != nil {
		return err
	}
	defer stopBridge()
	if err := h.withRoot(func(root *fsq.DeliveryRoot) error { return h.recover(ctx, root) }); err != nil {
		return err
	}
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		if err := h.withRoot(func(root *fsq.DeliveryRoot) error {
			if err := h.scan(ctx, root); err != nil {
				return err
			}
			return h.serveRequests(ctx, root)
		}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-h.conn.done:
			return fmt.Errorf("the agent exited: %w", h.conn.closedErr())
		case <-ticker.C:
		}
	}
}

// withRoot opens the root the host authenticated at start. A replaced root
// refuses instead of being followed.
func (h *Host) withRoot(fn func(*fsq.DeliveryRoot) error) error {
	root, err := fsq.OpenDeliveryRoot(h.cfg.Root, h.identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fn(root)
}

// handshake runs initialize and opens the session within one budget, so an
// agent that never answers does not hold the host lock forever.
func (h *Host) handshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	init, err := initialize(ctx, h.conn, h.cfg.Version)
	if err != nil {
		return err
	}
	h.agentName = strings.TrimSpace(init.AgentInfo.Name + " " + init.AgentInfo.Version)
	return h.openSession(ctx, init.loadSession())
}

// agentInit is the part of the agent's initialize answer the host reads.
type agentInit struct {
	ProtocolVersion json.RawMessage `json:"protocolVersion"`
	AgentInfo       struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agentInfo"`
	AgentCapabilities json.RawMessage `json:"agentCapabilities,omitempty"`
}

// loadSession reports agentCapabilities.loadSession; absent means false.
func (a agentInit) loadSession() bool {
	var caps struct {
		LoadSession bool `json:"loadSession"`
	}
	_ = json.Unmarshal(a.AgentCapabilities, &caps)
	return caps.LoadSession
}

// initialize sends the ACP initialize request and refuses a protocol
// version the host does not speak.
func initialize(ctx context.Context, c *conn, version string) (agentInit, error) {
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "amq-acp", "version": version},
	}, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return agentInit{}, actionRequired("the agent did not answer initialize within %s", handshakeTimeout)
		}
		return agentInit{}, fmt.Errorf("initialize the agent: %w", err)
	}
	var init agentInit
	if err := json.Unmarshal(raw, &init); err != nil {
		return agentInit{}, fmt.Errorf("initialize the agent: %w", err)
	}
	if string(init.ProtocolVersion) != strconv.Itoa(protocolVersion) {
		return agentInit{}, actionRequired("the agent answered ACP protocol version %s; amq-acp host speaks version %d", init.ProtocolVersion, protocolVersion)
	}
	return init, nil
}

// sessionRecord is session.json: the ACP session this host created, with
// the agent and working directory it was created for.
type sessionRecord struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Cwd       string `json:"cwd"`
	Created   string `json:"created"`
}

// openSession resumes the session this host recorded when the agent can load
// it, for the same agent and working directory; otherwise it opens a new one
// and records it. The host loads only a session id it recorded itself.
func (h *Host) openSession(ctx context.Context, canLoad bool) error {
	saved, err := h.readSessionRecord()
	if err != nil {
		_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %v; starting a new ACP session\n", err)
	}
	if saved != nil {
		switch {
		case saved.Agent == "" || saved.Cwd == "":
			_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: recorded session %s names no agent or directory; starting a new ACP session\n", saved.SessionID)
		case saved.Agent != h.cfg.Agent || saved.Cwd != h.cfg.Cwd:
			_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: recorded session %s belongs to %s in %s; starting a new ACP session\n", saved.SessionID, saved.Agent, saved.Cwd)
		case !canLoad:
			_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: the agent cannot load sessions; starting a new ACP session in place of %s\n", saved.SessionID)
		default:
			// The agent replays the session's history as session/update
			// notifications before it answers. No turn is in flight, so
			// onNotify collects none of it.
			_, err := h.conn.call(ctx, "session/load", map[string]any{"sessionId": saved.SessionID, "cwd": h.cfg.Cwd, "mcpServers": []any{}}, nil)
			if err == nil {
				h.sessionID, h.resumed = saved.SessionID, true
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return actionRequired("the agent did not answer session/load within %s", handshakeTimeout)
			}
			if authRequired(err) {
				return actionRequired("the agent needs a login: authenticate the harness once with its own CLI, then start the host again (%v)", err)
			}
			_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: the agent could not load session %s (%v); starting a new ACP session\n", saved.SessionID, err)
		}
	}

	raw, err := h.conn.call(ctx, "session/new", map[string]any{"cwd": h.cfg.Cwd, "mcpServers": []any{}}, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return actionRequired("the agent did not answer session/new within %s", handshakeTimeout)
		}
		if authRequired(err) {
			return actionRequired("the agent needs a login: authenticate the harness once with its own CLI, then start the host again (%v)", err)
		}
		return fmt.Errorf("open an ACP session: %w", err)
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &session); err != nil || session.SessionID == "" {
		return fmt.Errorf("open an ACP session: the agent returned no session id")
	}
	h.sessionID = session.SessionID
	return h.withRoot(func(root *fsq.DeliveryRoot) error {
		data, err := json.Marshal(sessionRecord{SessionID: h.sessionID, Agent: h.cfg.Agent, Cwd: h.cfg.Cwd, Created: time.Now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return err
		}
		_, err = root.WriteFileAtomic(filepath.Join("agents", h.cfg.Me, "extensions", extensionLayer), "session.json", append(data, '\n'), 0o600)
		return err
	})
}

// readSessionRecord reads session.json; a missing file is (nil, nil).
func (h *Host) readSessionRecord() (*sessionRecord, error) {
	var saved *sessionRecord
	err := h.withRoot(func(root *fsq.DeliveryRoot) error {
		data, err := root.ReadFile(filepath.Join("agents", h.cfg.Me, "extensions", extensionLayer, "session.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var rec sessionRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.SessionID == "" {
			return fmt.Errorf("session.json is unreadable")
		}
		saved = &rec
		return nil
	})
	return saved, err
}

// CheckResult is what amq-acp host --check prints: the agent's answer to
// initialize.
type CheckResult struct {
	ProtocolVersion int `json:"protocol_version"`
	AgentInfo       struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agent_info"`
	LoadSession       bool            `json:"load_session"`
	AgentCapabilities json.RawMessage `json:"agent_capabilities,omitempty"`
}

// Check probes an agent on in and out: initialize only, no session and no
// AMQ root. A refusal says why the host could not serve this agent.
func Check(ctx context.Context, in io.Reader, out io.Writer, version string) (CheckResult, error) {
	c := newConn(in, out, func(method string, _ json.RawMessage) func() (any, *rpcError) {
		return func() (any, *rpcError) {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
		}
	}, func(string, json.RawMessage) {})
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	init, err := initialize(ctx, c, version)
	if err != nil {
		return CheckResult{}, err
	}
	res := CheckResult{ProtocolVersion: protocolVersion, LoadSession: init.loadSession(), AgentCapabilities: init.AgentCapabilities}
	res.AgentInfo.Name, res.AgentInfo.Version = init.AgentInfo.Name, init.AgentInfo.Version
	return res, nil
}

// authRequired recognizes the ACP auth_required error by name; its numeric
// code is not fixed by the protocol pages.
func authRequired(err error) bool {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return false
	}
	text := strings.ToLower(rerr.Message + " " + string(rerr.Data))
	return strings.Contains(text, "auth_required") || strings.Contains(text, "authentication required")
}

// permissionRequest is the part of session/request_permission the host reads.
type permissionRequest struct {
	ToolCall json.RawMessage `json:"toolCall"`
	Options  []struct {
		OptionID string `json:"optionId"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

// onRequest takes the agent's requests on the connection's reader, so a
// request belongs to the turn in flight when it arrived; the function it
// returns gives the answer. Only session/request_permission is served, and
// it is always refused: the host's policy selects the first reject_once
// option, else the first reject_always, else cancelled. The host advertises
// no fs or terminal methods, so every other method is not found. During a
// remote request's turn the owner sees an approval that offers reject_once
// and may give it from the remote face first; the agent gets that same
// reject either way.
func (h *Host) onRequest(method string, params json.RawMessage) func() (any, *rpcError) {
	if method != "session/request_permission" {
		return func() (any, *rpcError) {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
		}
	}
	h.mu.Lock()
	turn := h.turn
	if turn != nil {
		turn.refused++
	}
	h.mu.Unlock()
	var req permissionRequest
	_ = json.Unmarshal(params, &req)
	first := func(kind string) int {
		for i, option := range req.Options {
			if option.Kind == kind && option.OptionID != "" {
				return i
			}
		}
		return -1
	}
	once := first("reject_once")
	policy := once
	if policy < 0 {
		policy = first("reject_always")
	}
	cancelled := map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
	selected := func(i int) map[string]any {
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": req.Options[i].OptionID}}
	}
	return func() (any, *rpcError) {
		if policy < 0 {
			return cancelled, nil
		}
		// The remote face offers only reject_once, never a wider scope.
		if turn != nil && turn.bridge != nil && once >= 0 && !h.askOwner(turn, req, once) {
			return cancelled, nil
		}
		return selected(policy), nil
	}
}

// onNotify collects the text of agent_message_chunk updates for the turn in
// flight. Thoughts, tool calls, plans and other updates are not the answer.
func (h *Host) onNotify(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var note struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &note) != nil {
		return
	}
	if note.Update.SessionUpdate != "agent_message_chunk" || note.Update.Content.Type != "text" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turn != nil && h.turn.sessionID == note.SessionID && h.turn.text.Len() < bodyLimit {
		h.turn.text.WriteString(note.Update.Content.Text)
	}
}

// recover finishes what an earlier host left in the journal before any new
// message runs. A turn whose prompt may have reached the agent is never
// prompted again.
func (h *Host) recover(ctx context.Context, root *fsq.DeliveryRoot) error {
	ids, err := listTurns(root, h.cfg.Me)
	if err != nil {
		return err
	}
	slices.Sort(ids)
	for _, id := range ids {
		rec, err := loadTurn(root, h.cfg.Me, id)
		if err != nil {
			_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %v; left as is\n", err)
			continue
		}
		if rec == nil || rec.State == stateReplied || rec.State == stateSkipped || rec.State == stateUndeliverable {
			continue
		}
		if err := h.advance(ctx, root, rec); err != nil {
			return err
		}
	}
	return nil
}

// scan runs every message in inbox/new, oldest first by file name (message
// ids begin with their creation time).
func (h *Host) scan(ctx context.Context, root *fsq.DeliveryRoot) error {
	entries, err := root.ReadDir(filepath.Join("agents", h.cfg.Me, "inbox", "new"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return nil
		}
		if err := h.take(ctx, root, name); err != nil {
			return err
		}
	}
	return nil
}

// take claims one inbox message and drives its turn to the end.
func (h *Host) take(ctx context.Context, root *fsq.DeliveryRoot, name string) error {
	msg, err := format.ReadMessageFileRoot(root, filepath.Join("agents", h.cfg.Me, "inbox", "new", name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		h.deadLetter(root, name, err)
		return nil
	}
	id := msg.Header.ID
	if id+".md" != name {
		if !h.unreadable[name] {
			h.unreadable[name] = true
			_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %s names message %s; left in the inbox\n", name, id)
		}
		return nil
	}
	rec, err := loadTurn(root, h.cfg.Me, id)
	if err != nil {
		return err
	}
	if rec == nil {
		now := time.Now()
		replyID, err := format.NewMessageID(now)
		if err != nil {
			return err
		}
		rec = &turnRecord{
			State:        stateClaimed,
			MessageID:    id,
			File:         name,
			From:         msg.Header.From,
			Thread:       msg.Header.Thread,
			Subject:      msg.Header.Subject,
			Kind:         msg.Header.Kind,
			ReplyID:      replyID,
			ReplyCreated: now.UTC().Format(time.RFC3339Nano),
			Created:      now.UTC().Format(time.RFC3339Nano),
		}
		if err := claimTurn(root, h.cfg.Me, *rec); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			if rec, err = loadTurn(root, h.cfg.Me, id); err != nil || rec == nil {
				return fmt.Errorf("turn record %s exists but is unreadable: %v", id, err)
			}
		}
	}
	return h.advance(ctx, root, rec)
}

// advance moves one record forward until it is replied or skipped.
func (h *Host) advance(ctx context.Context, root *fsq.DeliveryRoot, rec *turnRecord) error {
	var fatal error
	for {
		switch rec.State {
		case stateClaimed:
			msg, ok, err := h.takeMessage(root, rec)
			if err != nil {
				return err
			}
			switch reason := skipReason(msg); {
			case !ok:
				rec.State, rec.Reason = stateSkipped, "the message left the inbox before the host took it"
			case reason != "":
				rec.State, rec.Reason = stateSkipped, reason
			case msg.Header.ReplyTo != "" || msg.Header.ReplyProject != "":
				rec.State, rec.StopReason = stateAnswered, "unsupported"
				rec.ReplyBody = "This host answers only in its own AMQ root and cannot route a reply to another session or project. Send the message from this root."
			default:
				if fatal, err = h.runTurn(ctx, root, rec, msg); err != nil {
					return err
				}
				continue
			}
			if err := saveTurn(root, h.cfg.Me, *rec); err != nil {
				return err
			}
		case stateDispatching:
			// Only a record left by an earlier process is found here: the
			// prompt may have reached the agent, so it never runs again.
			rec.State, rec.StopReason = stateAnswered, "uncertain"
			rec.ReplyBody = "The host restarted during this turn; it did not run again."
			if err := saveTurn(root, h.cfg.Me, *rec); err != nil {
				return err
			}
		case stateAnswered:
			next := stateReplied
			if err := h.deliverReply(root, rec); err != nil {
				var permanent *permanentError
				if !errors.As(err, &permanent) {
					return err
				}
				// No retry can deliver this reply; record why and move on.
				next, rec.Reason = stateUndeliverable, err.Error()
				_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %v\n", err)
			}
			rec.State = next
			if err := saveTurn(root, h.cfg.Me, *rec); err != nil {
				return err
			}
		case stateReplied, stateSkipped, stateUndeliverable:
			return fatal
		default:
			return fmt.Errorf("turn record %s has unknown state %q", rec.MessageID, rec.State)
		}
	}
}

// skipReason is the loop guard. A message whose sender cannot receive a reply
// in this root (a bridged peer/handle, an empty or invalid handle) or that has
// no thread runs no turn. Only a question, a review request, a todo, a
// brainstorm, or a message with no kind runs a turn; answers, review
// responses, status and decisions are taken without a reply. A message with
// the host label is another host's reply.
func skipReason(msg format.Message) string {
	if err := fsq.ValidateHandle(msg.Header.From); err != nil {
		return fmt.Sprintf("sender %q cannot receive a reply here: %v", msg.Header.From, err)
	}
	if strings.TrimSpace(msg.Header.Thread) == "" {
		return "the message has no thread to reply on"
	}
	switch msg.Header.Kind {
	case "", format.KindQuestion, format.KindReviewRequest, format.KindTodo, format.KindBrainstorm:
	default:
		return "kind " + msg.Header.Kind + " runs no turn"
	}
	if slices.Contains(msg.Header.Labels, Label) {
		return "label " + Label + " marks another host's reply"
	}
	return ""
}

// takeMessage moves the claimed message to cur, writes the drained receipt,
// and reads it back. ok is false when the message is in neither new nor cur.
func (h *Host) takeMessage(root *fsq.DeliveryRoot, rec *turnRecord) (format.Message, bool, error) {
	// Every claimed record, fresh or found on restart, is durable before
	// its message moves.
	if err := syncJournal(root, h.cfg.Me); err != nil {
		return format.Message{}, false, err
	}
	if err := fsq.MoveNewToCur(root, h.cfg.Me, rec.File); err != nil {
		var committed *fsq.CommittedDurabilityError
		if !errors.As(err, &committed) && !os.IsNotExist(err) {
			return format.Message{}, false, err
		}
	}
	msg, err := format.ReadMessageFileRoot(root, filepath.Join("agents", h.cfg.Me, "inbox", "cur", rec.File))
	if err != nil {
		if os.IsNotExist(err) {
			return format.Message{}, false, nil
		}
		return format.Message{}, false, err
	}
	if err := receipt.EmitDeliveryRoot(root, receipt.New(rec.MessageID, rec.Thread, rec.From, h.cfg.Me, receipt.StageDrained, "")); err != nil {
		return format.Message{}, false, err
	}
	return msg, true, nil
}

// runTurn sends one session/prompt for msg and records the answer. err means
// the record could not be written; fatal means the reply is recorded but the
// host must stop after delivering it (the agent exited or ignored a cancel).
func (h *Host) runTurn(ctx context.Context, root *fsq.DeliveryRoot, rec *turnRecord, msg format.Message) (fatal, err error) {
	meta := map[string]any{"amq": map[string]string{"message_id": rec.MessageID, "thread": rec.Thread, "from": rec.From}}
	res, err := h.prompt(ctx, root, framePrompt(msg), meta, nil, func(id int64) error {
		rec.State, rec.RequestID, rec.SessionID = stateDispatching, id, h.sessionID
		return saveTurn(root, h.cfg.Me, *rec)
	})
	if err != nil {
		// No prompt byte was sent: the record stays claimed.
		rec.State = stateClaimed
		return nil, err
	}
	rec.State = stateAnswered
	rec.StopReason, rec.ReplyBody, fatal = replyFor(res.answer, res.cancelled, res.raw, res.err, h.cfg.AgentExit)
	if fatal == nil {
		fatal = res.stuck
	}
	if res.refused > 0 {
		rec.ReplyBody += fmt.Sprintf("\n\nThe host rejected %d tool call(s) the agent asked to run.", res.refused)
	}
	if err := saveTurn(root, h.cfg.Me, *rec); err != nil {
		return nil, err
	}
	return fatal, nil
}

// turnResult is how one prompt turn ended.
type turnResult struct {
	raw       json.RawMessage
	err       error
	cancelled string
	answer    string
	refused   int
	// stuck is set when the turn ended but its session/cancel write never
	// finished: the host must stop before any next turn.
	stuck error
}

// prompt sends one session/prompt with text and waits for it to end, cancel
// it on timeout or when ctx ends. sent runs after the request id is known and
// before any prompt byte; an error from sent, or a call that fails before
// it, is returned as err and nothing reached the agent. bridge is the remote
// request the turn serves, nil for a mailbox turn.
func (h *Host) prompt(ctx context.Context, root *fsq.DeliveryRoot, text string, meta map[string]any, bridge *bridgeTurn, sent func(id int64) error) (turnResult, error) {
	h.reloadSettings(root)
	h.setPresence(root, "busy")
	defer h.setPresence(root, "idle")

	collect := &turnCollect{sessionID: h.sessionID, cancelled: make(chan struct{}), done: make(chan struct{}), bridge: bridge}
	h.mu.Lock()
	h.turn = collect
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.turn = nil
		h.mu.Unlock()
		close(collect.done)
	}()

	params := map[string]any{
		"sessionId": h.sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	}
	if meta != nil {
		params["_meta"] = meta
	}
	type outcome struct {
		raw json.RawMessage
		err error
	}
	dispatched := make(chan error, 1)
	done := make(chan outcome, 1)
	go func() {
		ran := false
		raw, err := h.conn.call(context.Background(), "session/prompt", params, func(id int64) error {
			ran = true
			err := sent(id)
			dispatched <- err
			return err
		})
		if !ran {
			// The call ended before the hook ran.
			dispatched <- err
		}
		done <- outcome{raw, err}
	}()
	if err := <-dispatched; err != nil {
		return turnResult{}, err
	}

	timer := time.NewTimer(h.timeout)
	defer timer.Stop()
	busy := time.NewTicker(scanInterval)
	defer busy.Stop()
	var result outcome
	cancelled := ""
wait:
	for {
		select {
		case result = <-done:
			break wait
		case <-ctx.Done():
			cancelled = "the host was stopped"
			break wait
		case <-timer.C:
			cancelled = "the turn timed out after " + h.timeout.String()
			break wait
		case <-busy.C:
			// The owner's cancel applies only to the request in flight:
			// this loop runs exactly that turn.
			if bridge != nil && h.cancelRequested(root, bridge.ref) {
				cancelled = "the owner cancelled it"
				break wait
			}
			// A remote request that arrives during a turn is refused busy
			// at once, never queued behind it.
			h.refuseBusy(root)
		}
	}
	var stuck error
	if cancelled != "" {
		// A pending approval answers cancelled before the cancel goes out.
		// The write runs apart, so an agent that stopped reading its stdin
		// cannot hold the turn past the grace. session/cancel is
		// session-wide: the turn ends only once both the prompt response and
		// the write are done, so the cancel can never reach a later turn. A
		// write still pending at the grace stops the host instead.
		close(collect.cancelled)
		wrote := make(chan struct{})
		go func() {
			_ = h.conn.notify("session/cancel", map[string]string{"sessionId": h.sessionID})
			close(wrote)
		}()
		grace := time.NewTimer(cancelGrace)
		defer grace.Stop()
		answered := false
	drain:
		for !answered || wrote != nil {
			select {
			case result = <-done:
				answered = true
			case <-wrote:
				wrote = nil
			case <-grace.C:
				if !answered {
					result.err = fmt.Errorf("the agent did not answer the cancel within %s", cancelGrace)
				} else {
					stuck = fmt.Errorf("the session/cancel write did not finish within %s; the host stops so the cancel cannot reach a later turn", cancelGrace)
				}
				break drain
			}
		}
	}

	// A request published while this turn ran is refused busy now, before
	// the host is idle again: it never runs after the turn.
	h.refuseBusy(root)
	h.mu.Lock()
	defer h.mu.Unlock()
	return turnResult{raw: result.raw, err: result.err, cancelled: cancelled, answer: collect.text.String(), refused: collect.refused, stuck: stuck}, nil
}

// replyFor maps a turn's outcome to the stop label, the reply body, and
// whether the host must stop afterwards.
func replyFor(answer, cancelled string, raw json.RawMessage, callErr error, exitStatus func() string) (string, string, error) {
	answer = truncate(answer)
	if callErr != nil {
		var rerr *rpcError
		switch {
		case errors.As(callErr, &rerr):
			return "error", truncate("The agent failed this turn: " + rerr.Message), nil
		case errors.Is(callErr, errClosed):
			status := "status unknown"
			if exitStatus != nil {
				status = exitStatus()
			}
			return "error", "The agent exited during this turn (" + status + "); its effects are unknown.", callErr
		default:
			return "error", "The turn failed: " + callErr.Error() + "; its effects are unknown.", callErr
		}
	}
	var res struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(raw, &res)
	stop := stopLabel(res.StopReason)
	switch res.StopReason {
	case "end_turn":
		if strings.TrimSpace(answer) == "" {
			return stop, "The agent ended the turn without text.", nil
		}
		return stop, answer, nil
	case "refusal":
		return stop, joinNote(answer, "The agent refused this turn."), nil
	case "cancelled":
		who := cancelled
		if who == "" {
			who = "the agent cancelled it"
		}
		return stop, joinNote(answer, "The turn was cancelled: "+who+"."), nil
	default:
		return stop, joinNote(answer, "The turn stopped: "+res.StopReason+"."), nil
	}
}

// stopLabel keeps an agent-supplied stop reason to a safe label value.
func stopLabel(reason string) string {
	if reason == "" || strings.Trim(reason, "abcdefghijklmnopqrstuvwxyz_") != "" || len(reason) > 32 {
		return "other"
	}
	return reason
}

func joinNote(answer, note string) string {
	if strings.TrimSpace(answer) == "" {
		return note
	}
	return answer + "\n\n" + note
}

// truncate cuts text on a UTF-8 boundary so the reply fits one message.
func truncate(text string) string {
	if len(text) <= bodyLimit {
		return text
	}
	cut := bodyLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "\n[truncated]"
}

// framePrompt is the prompt text for one message: who sent it, on which
// thread, and the body.
func framePrompt(msg format.Message) string {
	h := msg.Header
	kind, priority := h.Kind, h.Priority
	if kind == "" {
		kind = "none"
	}
	if priority == "" {
		priority = format.PriorityNormal
	}
	return fmt.Sprintf("AMQ message %s from %s on thread %s\nSubject: %s    Kind: %s    Priority: %s\n\n%s", h.ID, h.From, h.Thread, h.Subject, kind, priority, msg.Body)
}

// deliverReply writes the recorded reply to the sender with amq-acp's
// message writer, plus the host's outbox/sent copy. The id and creation time
// come from the claim, so a replay is the same message and is not delivered
// twice.
func (h *Host) deliverReply(root *fsq.DeliveryRoot, rec *turnRecord) error {
	if err := fsq.ValidateHandle(rec.From); err != nil {
		return &permanentError{fmt.Errorf("reply to %s: sender %q: %w", rec.MessageID, rec.From, err)}
	}
	if strings.TrimSpace(rec.Thread) == "" {
		return &permanentError{fmt.Errorf("reply to %s: the message has no thread", rec.MessageID)}
	}
	kind := rec.Kind
	switch kind {
	case format.KindReviewRequest:
		kind = format.KindReviewResponse
	case format.KindQuestion:
		kind = format.KindAnswer
	}
	subject := rec.Subject
	switch {
	case subject == "":
		subject = "Re: (no subject)"
	case !strings.HasPrefix(strings.ToLower(subject), "re:"):
		subject = "Re: " + subject
	}
	data, err := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       rec.ReplyID,
			From:     h.cfg.Me,
			To:       []string{rec.From},
			Thread:   rec.Thread,
			Subject:  subject,
			Created:  rec.ReplyCreated,
			Refs:     []string{rec.MessageID},
			Priority: format.DefaultPriority(kind),
			Kind:     kind,
			Labels:   []string{Label, "acp-stop:" + rec.StopReason},
		},
		Body: rec.ReplyBody,
	}.Marshal()
	if err != nil {
		return &permanentError{fmt.Errorf("reply to %s: %w", rec.MessageID, err)}
	}
	if len(data) > format.MaxMessageSize {
		return &permanentError{fmt.Errorf("reply to %s exceeds the maximum AMQ message size", rec.MessageID)}
	}
	created, err := time.Parse(time.RFC3339Nano, rec.ReplyCreated)
	if err != nil {
		return &permanentError{fmt.Errorf("turn record %s has an unreadable reply time", rec.MessageID)}
	}
	if _, err := acp.DeliverPrepared(root, rec.From, rec.Thread, rec.ReplyID, created, data); err != nil {
		return fmt.Errorf("reply to %s: %w", rec.MessageID, err)
	}
	_, _ = root.WriteFileAtomic(filepath.Join("agents", h.cfg.Me, "outbox", "sent"), rec.ReplyID+".md", data, 0o600)
	return nil
}

// permanentError is a reply that no retry can deliver: the record itself
// cannot form a deliverable message. Any other delivery error is I/O and
// stops the host so the next start retries it.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// deadLetter moves an inbox file that does not parse to the DLQ with a dlq
// receipt, the path amq drain takes for a corrupt message. A move that fails
// leaves the file in place and is logged once.
func (h *Host) deadLetter(root *fsq.DeliveryRoot, name string, parseErr error) {
	id := strings.TrimSuffix(name, ".md")
	if _, err := fsq.MoveToDLQ(root, h.cfg.Me, name, id, "parse_error", parseErr.Error()); err != nil {
		var committed *fsq.CommittedDurabilityError
		if !errors.As(err, &committed) {
			if !h.unreadable[name] {
				h.unreadable[name] = true
				_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %s does not parse (%v) and could not move to the DLQ: %v\n", name, parseErr, err)
			}
			return
		}
	}
	_ = receipt.EmitDeliveryRoot(root, receipt.New(id, "", "", h.cfg.Me, receipt.StageDLQ, parseErr.Error()))
	_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: %s does not parse (%v); moved to the DLQ\n", name, parseErr)
}

// reloadSettings re-reads turn_timeout from the host's settings.json at the
// start of each turn. A missing file means the default; a refused edit keeps
// the last good value.
func (h *Host) reloadSettings(root *fsq.DeliveryRoot) {
	data, err := root.ReadFile(filepath.Join("agents", h.cfg.Me, "extensions", extensionLayer, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		h.timeout = defaultTurnTimeout
		return
	}
	var settings struct {
		TurnTimeout string `json:"turn_timeout"`
	}
	if err == nil {
		err = json.Unmarshal(data, &settings)
	}
	if err == nil && settings.TurnTimeout == "" {
		h.timeout = defaultTurnTimeout
		return
	}
	if err == nil {
		var d time.Duration
		if d, err = time.ParseDuration(settings.TurnTimeout); err == nil && d <= 0 {
			err = errors.New("turn_timeout must be positive")
		}
		if err == nil {
			h.timeout = d
			return
		}
	}
	_, _ = fmt.Fprintf(h.cfg.Log, "amq-acp host: settings.json refused (%v); turn_timeout stays %s\n", err, h.timeout)
}

func (h *Host) setPresence(root *fsq.DeliveryRoot, status string) {
	note := "acp"
	if h.agentName != "" {
		note += " " + h.agentName
	}
	_ = presence.WriteDeliveryRoot(root, presence.New(h.cfg.Me, status, note, time.Now()))
}
