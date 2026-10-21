package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Timing shared by the terminal backends. A backend's timing fields override
// these only when positive.
const (
	backendCommandTimeout = 5 * time.Second
	backendCreateTimeout  = 30 * time.Second
	backendHealthTimeout  = 15 * time.Second
	backendHealthPoll     = 100 * time.Millisecond
)

// validateCreate checks the request every backend Create needs: a pinned root,
// a valid plan whose agents share one UUID launch nonce, and existing mailboxes.
func validateCreate(req CreateRequest) error {
	if strings.TrimSpace(req.ProjectRoot) == "" || strings.TrimSpace(req.Session) == "" || req.Root == nil {
		return fmt.Errorf("project root, session, and pinned root are required")
	}
	if err := req.Plan.Validate(); err != nil {
		return err
	}
	if _, err := canonicalIdentity(req.ProjectRoot); err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	handles := make([]string, len(req.Plan.Agents))
	nonce := req.Plan.Agents[0].LaunchNonce
	if !validUUID(nonce) {
		return fmt.Errorf("launch nonce must be a UUID")
	}
	for i, agent := range req.Plan.Agents {
		handles[i] = agent.Handle
		if agent.LaunchNonce != nonce {
			return fmt.Errorf("agent %q has a different launch nonce", agent.Handle)
		}
	}
	return fsq.ValidateExistingMailboxLayout(req.Root, handles...)
}

// agentCommand is the shell line a backend types into an agent's terminal:
// `amq coop exec` for the agent, carrying its launch nonce.
func agentCommand(req CreateRequest, agent AgentPlan) string {
	amq := req.AMQPath
	if strings.TrimSpace(amq) == "" {
		amq = "amq"
	}
	env := cloneEnv(agent.EnvOverlay)
	if env == nil {
		env = make(map[string]string, 1)
	}
	env[InternalLaunchNonceEnv] = agent.LaunchNonce
	return commandLine(agent.Cwd, env, coopExecArgv(amq, req.Root.Base(), agent.Handle, agent.Argv, agent.Execution))
}

// sleepContext waits for delay or until ctx ends.
func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backendTiming holds the sleep seam and timing overrides of a backend that
// polls its terminals for health. Zero values select the shared defaults.
type backendTiming struct {
	sleep          func(context.Context, time.Duration) error
	createTimeout  time.Duration
	commandTimeout time.Duration
	healthTimeout  time.Duration
	healthPoll     time.Duration
}

func (t *backendTiming) doSleep(ctx context.Context, delay time.Duration) error {
	if t.sleep != nil {
		return t.sleep(ctx, delay)
	}
	return sleepContext(ctx, delay)
}

func (t *backendTiming) createCallContext() (context.Context, context.CancelFunc) {
	timeout := t.createTimeout
	if timeout <= 0 {
		timeout = backendCreateTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

func (t *backendTiming) commandCallContext() (context.Context, context.CancelFunc) {
	timeout := t.commandTimeout
	if timeout <= 0 {
		timeout = backendCommandTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

// inspectCallContext bounds one inspect or input call. It ignores the
// commandTimeout override.
func (t *backendTiming) inspectCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), backendCommandTimeout)
}

// healthBudget returns how long to wait for health and how often to poll.
func (t *backendTiming) healthBudget() (timeout, poll time.Duration) {
	timeout, poll = t.healthTimeout, t.healthPoll
	if timeout <= 0 {
		timeout = backendHealthTimeout
	}
	if poll <= 0 {
		poll = backendHealthPoll
	}
	return timeout, poll
}

// staggerPlacement waits the placement stagger before the agent at index; the
// first agent starts at once.
func (t *backendTiming) staggerPlacement(preview PlacementPreview, index int) error {
	delay := placementStaggerDuration(&preview.Effective)
	if delay <= 0 || index == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), delay+time.Second)
	defer cancel()
	return t.doSleep(ctx, delay)
}

// inspectBinding runs one bounded inspect. An inspect error is unknown state
// that needs action, never absence.
func inspectBinding(binding BindingRecord, inspect func(context.Context, BindingRecord) (InspectResult, error)) (InspectResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
	defer cancel()
	state, err := inspect(ctx, binding)
	if err != nil {
		return InspectResult{Status: InspectUnknown, Evidence: err.Error(), ActionRequired: true}, nil
	}
	return state, nil
}

// checkBindingContext refuses a binding made by another backend, profile,
// host, or backend instance. instance returns the live instance identity, or
// "" when the backend is unreachable.
func checkBindingContext(binding BindingRecord, backend string, profile Profile, hostname func() (string, error), instance func() string, unreachable string) error {
	host, err := hostname()
	if err != nil {
		return fmt.Errorf("resolve %s host identity: %w", backend, err)
	}
	if binding.Backend != backend || binding.Profile != profile.Identity() || binding.HostIdentity != host {
		return fmt.Errorf("%s binding belongs to a different backend context", backend)
	}
	live := instance()
	if live == "" {
		return errors.New(unreachable)
	}
	if binding.InstanceIdentity != live {
		return fmt.Errorf("%s binding belongs to a different backend context", backend)
	}
	return nil
}

// ownedTerminalClose is the close sequence of a backend whose binding owns one
// container of per-agent terminals: inspect, close each owned terminal, then
// prove none survive. Every uncertain step is action-required.
type ownedTerminalClose struct {
	backend   string // "cmux"
	container string // "workspace"
	terminal  string // "surface"
	inspect   func(context.Context) (InspectResult, error)
	owned     []string
	// containerID parses the bound container. inspect parses it first, so an
	// error here means the binding changed underneath and stays fail-closed.
	containerID   func() (string, error)
	closeTerminal func(context.Context, string) error
	targetMissing func(error) bool
	stillLive     func(ctx context.Context, containerID string, owned []string) (int, error)
	// closed runs after every owned terminal is proven gone.
	closed func(containerID string)
}

func (c ownedTerminalClose) run() (CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), backendCommandTimeout)
	defer cancel()
	inspection, err := c.inspect(ctx)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if inspection.Status == InspectAbsent {
		return CloseResult{Outcome: OutcomeClosed, Reason: c.backend + " " + c.container + " already absent"}, nil
	}
	if len(c.owned) == 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: c.backend + " binding has no " + c.terminal + " identities"}, nil
	}
	containerID, err := c.containerID()
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	for _, id := range c.owned {
		if closeErr := c.closeTerminal(ctx, id); closeErr != nil && !c.targetMissing(closeErr) {
			return CloseResult{}, fmt.Errorf("close %s %s %s: %w", c.backend, c.terminal, id, closeErr)
		}
	}
	live, err := c.stillLive(ctx, containerID, c.owned)
	if err != nil {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: err.Error()}, nil
	}
	if live > 0 {
		return CloseResult{Outcome: OutcomeActionRequired, Reason: "owned " + c.backend + " " + c.terminal + " still alive after close"}, nil
	}
	if c.closed != nil {
		c.closed(containerID)
	}
	return CloseResult{Outcome: OutcomeClosed, Reason: c.backend + " " + c.terminal + "s closed"}, nil
}
