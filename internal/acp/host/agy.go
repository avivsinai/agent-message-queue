package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// ProtocolAgyStream drives agy headless over its stream-json mode: one NDJSON
// "user" line per turn in, "init", "step_update" and "result" events out.
const ProtocolAgyStream = "agy-stream"

// AgyAutonomyNotice is the agy counterpart of AutonomyNotice.
const AgyAutonomyNotice = "agy denies every tool call that needs a permission unless its own arguments or its permissions.allow settings grant it, for example --dangerously-skip-permissions; the host adds no permission flag."

// agyStreamArgs puts agy in headless stream-json mode. -p takes an empty
// value: a bare -p swallows the next flag.
var agyStreamArgs = []string{"--input-format", "stream-json", "--output-format", "stream-json", "-p="}

// agyOwnedFlags are the flags the host sets itself; an operator argv that
// names one is refused.
var agyOwnedFlags = []string{"--conversation", "-c", "--continue", "-p", "--print", "--prompt", "-i", "--input-format", "--output-format"}

// AgyArgvRefusal refuses an operator argv that sets a flag the host owns.
func AgyArgvRefusal(args []string) error {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		for _, owned := range agyOwnedFlags {
			if name == owned {
				return fmt.Errorf("agy argument %s is set by the host; remove it", owned)
			}
		}
	}
	return nil
}

// AgentProcess is one agent process the host started.
type AgentProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Interrupt sends the agent SIGINT.
	Interrupt() error
	// Stop closes stdin, waits briefly for the exit, then kills what is
	// left of the process group.
	Stop()
}

// Spawner starts the agent with extra appended to the operator's argv.
type Spawner func(extra []string) (AgentProcess, error)

// RunAgy serves the inbox through agy until ctx ends. The host starts agy
// itself, because agy fixes its conversation at process start: a resume, a
// cancel or an exit between turns respawns it with --conversation.
func (h *Host) RunAgy(ctx context.Context, spawn Spawner) error {
	h.label, h.notice = "agy conversation", AgyAutonomyNotice
	h.refusedNote = "agy denied %d tool call(s) it needed; its permissions.allow settings or its arguments grant them."
	sess := &agySession{h: h, spawn: spawn}
	defer func() {
		if sess.cur != nil {
			sess.cur.p.Stop()
		}
	}()
	return h.run(ctx, sess)
}

type agyEvent struct {
	Event          string          `json:"event"`
	ConversationID string          `json:"conversation_id"`
	Init           json.RawMessage `json:"init"`
	Result         *agyResult      `json:"result"`
}

type agyResult struct {
	ConversationID string            `json:"conversation_id"`
	Status         string            `json:"status"`
	Response       string            `json:"response"`
	Error          string            `json:"error"`
	DeniedActions  []json.RawMessage `json:"denied_actions"`
}

// agyProc is one running agy and its reader.
type agyProc struct {
	p       AgentProcess
	init    chan agyEvent
	results chan agyResult
	exited  chan struct{}
}

func startAgyProc(spawn Spawner, extra []string) (*agyProc, error) {
	p, err := spawn(append(append([]string(nil), agyStreamArgs...), extra...))
	if err != nil {
		return nil, err
	}
	proc := &agyProc{p: p, init: make(chan agyEvent, 1), results: make(chan agyResult, 4), exited: make(chan struct{})}
	go proc.read()
	return proc, nil
}

func (proc *agyProc) read() {
	defer close(proc.exited)
	scanner := bufio.NewScanner(proc.p.Stdout())
	scanner.Buffer(make([]byte, 0, 64*1024), format.MaxMessageSize+1024)
	for scanner.Scan() {
		var ev agyEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Event == "init":
			select {
			case proc.init <- ev:
			default:
			}
		case ev.Event == "result" && ev.Result != nil:
			proc.results <- *ev.Result
		}
	}
}

// waitInit waits for the init event, which names the conversation.
func (proc *agyProc) waitInit(ctx context.Context) (string, error) {
	select {
	case ev := <-proc.init:
		if ev.ConversationID == "" {
			return "", fmt.Errorf("agy init named no conversation")
		}
		return ev.ConversationID, nil
	case <-proc.exited:
		return "", fmt.Errorf("%w before its init event", errClosed)
	case <-ctx.Done():
		return "", actionRequired("agy did not send its init event within %s", handshakeTimeout)
	}
}

// agySession speaks agy's stream-json. One process serves turns until it
// exits; the next turn then respawns it on the same conversation.
type agySession struct {
	h     *Host
	spawn Spawner
	cur   *agyProc
	id    string
}

func (s *agySession) agentName() string { return "agy" }

// done never closes: agy exiting between turns is respawned, and a failed
// respawn fails the next turn instead.
func (s *agySession) done() <-chan struct{} { return nil }
func (s *agySession) err() error            { return nil }

// start runs agy on conversation (empty for a new one) and waits for init.
func (s *agySession) start(ctx context.Context, conversation string) (string, error) {
	var extra []string
	if conversation != "" {
		extra = []string{"--conversation", conversation}
	}
	proc, err := startAgyProc(s.spawn, extra)
	if err != nil {
		return "", err
	}
	id, err := proc.waitInit(ctx)
	if err != nil {
		proc.p.Stop()
		return "", err
	}
	s.cur = proc
	return id, nil
}

func (s *agySession) open(ctx context.Context, recorded *sessionRecord) (string, bool, error) {
	want := ""
	if recorded != nil {
		want = recorded.SessionID
	}
	id, err := s.start(ctx, want)
	if err != nil {
		return "", false, err
	}
	s.id = id
	if want != "" && id != want {
		// agy runs an unknown conversation id under a new one.
		_, _ = fmt.Fprintf(s.h.cfg.Log, "amq-acp host: agy did not resume conversation %s; it runs as %s\n", want, id)
		return id, false, nil
	}
	return id, want != "", nil
}

// ensure respawns agy on the current conversation when its process has
// exited. A respawn that lands on another conversation is recorded.
func (s *agySession) ensure() error {
	if s.cur != nil {
		select {
		case <-s.cur.exited:
			s.cur.p.Stop()
			s.cur = nil
		default:
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	id, err := s.start(ctx, s.id)
	if err != nil {
		return err
	}
	if id != s.id {
		_, _ = fmt.Fprintf(s.h.cfg.Log, "amq-acp host: agy did not resume conversation %s; it runs as %s\n", s.id, id)
		s.id = id
		s.h.sessionID = id
		if err := s.h.recordSession(); err != nil {
			return err
		}
	}
	return nil
}

func (s *agySession) send(text string, _ map[string]any, sent func(id int64) error) (<-chan turnEnd, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	proc := s.cur
	line, err := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"role": "user", "content": text}})
	if err != nil {
		return nil, err
	}
	s.h.mu.Lock()
	turn := s.h.turn
	s.h.mu.Unlock()
	dispatched := make(chan error, 1)
	ended := make(chan turnEnd, 1)
	go func() {
		if err := sent(0); err != nil {
			dispatched <- err
			return
		}
		if _, err := proc.p.Stdin().Write(append(line, '\n')); err != nil {
			// The line may be partly written: the turn is uncertain.
			dispatched <- nil
			ended <- turnEnd{err: fmt.Errorf("%w: %v", errClosed, err)}
			return
		}
		dispatched <- nil
		select {
		case res := <-proc.results:
			ended <- s.finish(turn, res)
		case <-proc.exited:
			// The reader queues a result before it sees the exit.
			select {
			case res := <-proc.results:
				ended <- s.finish(turn, res)
			default:
				ended <- turnEnd{err: errClosed}
			}
		}
	}()
	if err := <-dispatched; err != nil {
		return nil, err
	}
	return ended, nil
}

// finish maps a result to the turn's end, and puts the turn's answer and its
// denied tool calls into the turn.
func (s *agySession) finish(turn *turnCollect, res agyResult) turnEnd {
	s.h.mu.Lock()
	if turn != nil {
		turn.text.WriteString(cutUTF8(res.Response, bodyLimit))
		turn.refused += len(res.DeniedActions)
	}
	s.h.mu.Unlock()
	switch {
	case res.Status == "SUCCESS":
		return turnEnd{stop: "end_turn"}
	case res.Error == "interrupted":
		return turnEnd{stop: "cancelled"}
	default:
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = "agy reported status " + res.Status
		}
		return turnEnd{err: &rpcError{Message: msg}}
	}
}

// cancel interrupts agy, which ends the turn with an interrupted result and
// exits; the next turn respawns it. A signal is delivered when Interrupt
// returns, so nothing of the cancel is left to reach a later turn.
func (s *agySession) cancel() <-chan struct{} {
	delivered := make(chan struct{})
	if s.cur != nil {
		_ = s.cur.p.Interrupt()
	}
	close(delivered)
	return delivered
}

// AgyCheckResult is what amq-acp host --check prints for agy: its init event.
type AgyCheckResult struct {
	Protocol       string          `json:"protocol"`
	ConversationID string          `json:"conversation_id"`
	Init           json.RawMessage `json:"init,omitempty"`
}

// CheckAgy starts agy in stream-json mode, reads its init event, and stops
// it. No AMQ root is involved.
func CheckAgy(ctx context.Context, spawn Spawner) (AgyCheckResult, error) {
	proc, err := startAgyProc(spawn, nil)
	if err != nil {
		return AgyCheckResult{}, err
	}
	defer proc.p.Stop()
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	select {
	case ev := <-proc.init:
		return AgyCheckResult{Protocol: ProtocolAgyStream, ConversationID: ev.ConversationID, Init: ev.Init}, nil
	case <-proc.exited:
		return AgyCheckResult{}, errors.New("agy exited before its init event")
	case <-ctx.Done():
		return AgyCheckResult{}, fmt.Errorf("agy did not send its init event within %s", handshakeTimeout)
	}
}
