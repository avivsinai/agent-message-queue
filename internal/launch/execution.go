package launch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const ExecutionTicketVersion = 1

const executionDirectory = "meta/launch/executions"

const (
	executionLeaseRetryInterval = 50 * time.Millisecond
	executionLeaseWaitTimeout   = 10 * time.Second
)

type ExecutionState string

const (
	ExecutionPending          ExecutionState = "pending"
	ExecutionIdentityAcquired ExecutionState = "identity_acquired"
	ExecutionSpawnAttempted   ExecutionState = "spawn_attempted"
	ExecutionAcknowledged     ExecutionState = "acknowledged"
)

// ExecutionTicket is the durable, nonce-bound handoff between planning and
// the process which actually starts a command. It is evidence, not execution
// authority: writes require the live launch lease and the matching handle lock.
type ExecutionTicket struct {
	Version int `json:"version"`

	Handle          string      `json:"handle"`
	LaunchNonce     string      `json:"launch_nonce"`
	Mode            AdapterMode `json:"mode"`
	Provider        string      `json:"provider"`
	ProviderVersion string      `json:"provider_version,omitempty"`
	ConversationID  string      `json:"conversation_id,omitempty"`
	PreSpawnAcquire bool        `json:"pre_spawn_acquire,omitempty"`
	EvidenceRefs    []string    `json:"evidence_refs,omitempty"`
	Backend         string      `json:"backend,omitempty"`
	Profile         string      `json:"profile,omitempty"`

	ProjectRoot     string `json:"project_root"`
	ProjectIdentity string `json:"project_identity"`
	SessionRoot     string `json:"session_root"`
	SessionIdentity string `json:"session_identity"`
	Cwd             string `json:"cwd"`
	CwdIdentity     string `json:"cwd_identity"`

	ProviderExecutable         string `json:"provider_executable"`
	ProviderExecutableIdentity string `json:"provider_executable_identity"`
	AMQExecutable              string `json:"amq_executable"`
	AMQExecutableIdentity      string `json:"amq_executable_identity"`
	InjectorExecutable         string `json:"injector_executable,omitempty"`
	InjectorExecutableIdentity string `json:"injector_executable_identity,omitempty"`

	TargetArgv    []string                 `json:"target_argv"`
	DynamicArgv   []DynamicArg             `json:"dynamic_argv,omitempty"`
	InitialInput  *PlannedInitialInput     `json:"initial_input,omitempty"`
	TargetEnv     map[string]string        `json:"target_env,omitempty"`
	EnvDigest     string                   `json:"env_digest"`
	State         ExecutionState           `json:"state"`
	Reason        string                   `json:"reason,omitempty"`
	Execution     *PrepareExecutionOptions `json:"execution,omitempty"`
	CallerContext map[string]string        `json:"caller_context,omitempty"`
}

type ExecutionTicketRequest struct {
	Handle, LaunchNonce               string
	Mode                              AdapterMode
	Provider, ProviderVersion         string
	ConversationID                    string
	PreSpawnAcquire                   bool
	EvidenceRefs                      []string
	Backend, Profile                  string
	ProjectRoot, SessionRoot, Cwd     string
	ProviderExecutable, AMQExecutable string
	TargetArgv                        []string
	DynamicArgv                       []DynamicArg
	InitialInput                      *PlannedInitialInput
	TargetEnv                         map[string]string
	State                             ExecutionState
	Reason                            string
	Execution                         *PrepareExecutionOptions
	CallerContext                     map[string]string
}

type ExecutionEnvelope struct {
	Cwd, AMQExecutable, ProviderExecutable string
	TargetArgv, Environment                []string
	Execution                              *PrepareExecutionOptions
}

func ExecutionTicketPath(sessionRoot, handle string) string {
	return filepath.Join(sessionRoot, executionDirectory, handle+".json")
}

// NewExecutionTicket canonicalizes every filesystem input and snapshots its
// physical identity before the ticket can be persisted.
func NewExecutionTicket(request ExecutionTicketRequest) (ExecutionTicket, error) {
	if err := fsq.ValidateHandle(request.Handle); err != nil {
		return ExecutionTicket{}, err
	}
	if !validUUID(request.LaunchNonce) {
		return ExecutionTicket{}, fmt.Errorf("launch nonce must be a UUID")
	}
	if request.Mode != AdapterModeMint && request.Mode != AdapterModeCapture && request.Mode != AdapterModeUnsupported {
		return ExecutionTicket{}, fmt.Errorf("invalid execution mode %q", request.Mode)
	}
	project, projectID, err := canonicalDirectory(request.ProjectRoot)
	if err != nil {
		return ExecutionTicket{}, fmt.Errorf("project root: %w", err)
	}
	session, sessionID, err := canonicalDirectory(request.SessionRoot)
	if err != nil {
		return ExecutionTicket{}, fmt.Errorf("session root: %w", err)
	}
	cwd, cwdID, err := canonicalDirectory(request.Cwd)
	if err != nil {
		return ExecutionTicket{}, fmt.Errorf("cwd: %w", err)
	}
	provider, providerID, err := canonicalFile(request.ProviderExecutable)
	if err != nil {
		return ExecutionTicket{}, fmt.Errorf("provider executable: %w", err)
	}
	if pathWithin(provider, project) {
		return ExecutionTicket{}, fmt.Errorf("provider executable resolves inside the project")
	}
	amq, amqID, err := canonicalFile(request.AMQExecutable)
	if err != nil {
		return ExecutionTicket{}, fmt.Errorf("amq executable: %w", err)
	}
	execution := clonePrepareExecutionOptions(request.Execution)
	if execution != nil {
		canonical := CanonicalExecutionOptions(execution)
		execution = &canonical
	}
	var injector, injectorID string
	if execution != nil && execution.InjectorVia != "" {
		injector, injectorID, err = canonicalFile(execution.InjectorVia)
		if err != nil {
			return ExecutionTicket{}, fmt.Errorf("injector executable: %w", err)
		}
	}
	env := cloneExecutionEnv(request.TargetEnv)
	digest, err := executionEnvDigest(env)
	if err != nil {
		return ExecutionTicket{}, err
	}
	ticket := ExecutionTicket{
		Version: ExecutionTicketVersion, Handle: request.Handle, LaunchNonce: request.LaunchNonce, Mode: request.Mode,
		Provider: request.Provider, ProviderVersion: request.ProviderVersion, ConversationID: request.ConversationID,
		PreSpawnAcquire: request.PreSpawnAcquire, EvidenceRefs: append([]string(nil), request.EvidenceRefs...),
		Backend: request.Backend, Profile: request.Profile,
		ProjectRoot: project, ProjectIdentity: projectID, SessionRoot: session, SessionIdentity: sessionID,
		Cwd: cwd, CwdIdentity: cwdID, ProviderExecutable: provider, ProviderExecutableIdentity: providerID,
		AMQExecutable: amq, AMQExecutableIdentity: amqID, TargetArgv: append([]string(nil), request.TargetArgv...),
		DynamicArgv:  append([]DynamicArg(nil), request.DynamicArgv...),
		InitialInput: clonePlannedInitialInput(request.InitialInput),
		TargetEnv:    env, EnvDigest: digest, State: request.State, Reason: request.Reason,
		Execution: execution, InjectorExecutable: injector, InjectorExecutableIdentity: injectorID,
		CallerContext: cloneCallerContext(request.CallerContext),
	}
	if ticket.State == "" {
		ticket.State = ExecutionPending
	}
	if err := ticket.Validate(); err != nil {
		return ExecutionTicket{}, err
	}
	return ticket, nil
}

func (ticket ExecutionTicket) Validate() error {
	if ticket.Version != ExecutionTicketVersion {
		return fmt.Errorf("unsupported execution ticket version %d", ticket.Version)
	}
	if err := fsq.ValidateHandle(ticket.Handle); err != nil {
		return err
	}
	if !validUUID(ticket.LaunchNonce) {
		return fmt.Errorf("launch nonce must be a UUID")
	}
	if ticket.Mode != AdapterModeMint && ticket.Mode != AdapterModeCapture && ticket.Mode != AdapterModeUnsupported {
		return fmt.Errorf("invalid execution mode %q", ticket.Mode)
	}
	if strings.TrimSpace(ticket.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if ticket.ConversationID != "" && !validUUID(ticket.ConversationID) {
		return fmt.Errorf("conversation identity must be a UUID")
	}
	if ticket.PreSpawnAcquire {
		if ticket.Mode != AdapterModeCapture || ticket.Provider != CursorProvider || ticket.ProviderVersion != cursorCaptureVersion ||
			strings.TrimSpace(ticket.Backend) == "" || strings.TrimSpace(ticket.Profile) == "" {
			return fmt.Errorf("pre-spawn identity acquisition requires a versioned cursor capture ticket")
		}
	} else if (ticket.Backend == "") != (ticket.Profile == "") {
		return fmt.Errorf("execution backend identity is incomplete")
	} else if ticket.Mode == AdapterModeCapture && ticket.Provider == CodexProvider && ticket.ProviderVersion == codexCaptureVersion && ticket.Backend == "" {
		return fmt.Errorf("codex notify capture requires execution backend identity")
	} else if ticket.Backend != "" && (ticket.Mode != AdapterModeCapture || ticket.Provider != CodexProvider || ticket.ProviderVersion != codexCaptureVersion) {
		return fmt.Errorf("execution backend identity is unsupported without pre-spawn acquisition")
	}
	for name, value := range map[string]string{"project root": ticket.ProjectRoot, "project identity": ticket.ProjectIdentity, "session root": ticket.SessionRoot, "session identity": ticket.SessionIdentity, "cwd": ticket.Cwd, "cwd identity": ticket.CwdIdentity, "provider executable": ticket.ProviderExecutable, "provider executable identity": ticket.ProviderExecutableIdentity, "amq executable": ticket.AMQExecutable, "amq executable identity": ticket.AMQExecutableIdentity, "environment digest": ticket.EnvDigest} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if ticket.Execution != nil && ticket.Execution.InjectorVia != "" {
		if ticket.InjectorExecutable == "" || ticket.InjectorExecutableIdentity == "" {
			return fmt.Errorf("injector executable identity is required")
		}
	} else if ticket.InjectorExecutable != "" || ticket.InjectorExecutableIdentity != "" {
		return fmt.Errorf("injector executable identity exists without injector via")
	}
	if len(ticket.TargetArgv) == 0 || strings.TrimSpace(ticket.TargetArgv[0]) == "" {
		return fmt.Errorf("target argv is required")
	}
	for i, arg := range ticket.TargetArgv {
		if strings.ContainsRune(arg, 0) || (arg == "" && !ticketInitialInputOwnsArg(ticket, i)) {
			return fmt.Errorf("target argv[%d] is invalid", i)
		}
	}
	if err := validateTicketInitialInput(ticket); err != nil {
		return err
	}
	if err := ValidateCallerContext(ticket.CallerContext); err != nil {
		return err
	}
	if ticket.Mode == AdapterModeCapture && ticket.Provider == CodexProvider && ticket.ProviderVersion == codexCaptureVersion {
		if err := validateCodexNotifyTargetArgv(ticket); err != nil {
			return err
		}
	}
	seenDynamic := make(map[int]struct{}, len(ticket.DynamicArgv))
	conversationSlots := 0
	for i, dynamic := range ticket.DynamicArgv {
		if dynamic.Index <= 0 || dynamic.Index >= len(ticket.TargetArgv) {
			return fmt.Errorf("dynamic argv[%d] index is invalid", i)
		}
		if _, exists := seenDynamic[dynamic.Index]; exists {
			return fmt.Errorf("dynamic argv[%d] index is duplicated", i)
		}
		seenDynamic[dynamic.Index] = struct{}{}
		switch dynamic.Kind {
		case DynamicArgLaunchNonce:
			if ticket.TargetArgv[dynamic.Index] != ticket.LaunchNonce {
				return fmt.Errorf("dynamic launch nonce slot does not match ticket")
			}
		case DynamicArgConversationID:
			conversationSlots++
			expected := ticket.ConversationID
			if ticket.PreSpawnAcquire {
				expected = preSpawnConversationPlaceholder
			}
			if expected == "" || ticket.TargetArgv[dynamic.Index] != expected {
				return fmt.Errorf("dynamic conversation slot does not match ticket")
			}
		default:
			return fmt.Errorf("dynamic argv[%d] kind is invalid", i)
		}
	}
	if ticket.PreSpawnAcquire && conversationSlots != 1 {
		return fmt.Errorf("pre-spawn execution requires exactly one dynamic conversation slot")
	}
	seenEvidence := make(map[string]struct{}, len(ticket.EvidenceRefs))
	for _, id := range ticket.EvidenceRefs {
		if !validDigest(id) {
			return fmt.Errorf("execution evidence ref is invalid")
		}
		if _, exists := seenEvidence[id]; exists {
			return fmt.Errorf("execution evidence ref is duplicated")
		}
		seenEvidence[id] = struct{}{}
	}
	if digest, err := executionEnvDigest(ticket.TargetEnv); err != nil || digest != ticket.EnvDigest {
		if err != nil {
			return err
		}
		return fmt.Errorf("environment digest does not match target environment")
	}
	switch ticket.State {
	case ExecutionPending:
		if ticket.PreSpawnAcquire && (ticket.ConversationID != "" || len(ticket.EvidenceRefs) != 0) {
			return fmt.Errorf("pending pre-spawn ticket must not contain acquired identity")
		}
	case ExecutionIdentityAcquired, ExecutionSpawnAttempted, ExecutionAcknowledged:
		if strings.TrimSpace(ticket.Reason) == "" {
			return fmt.Errorf("reason is required for execution state %q", ticket.State)
		}
		if ticket.PreSpawnAcquire && (!validUUID(ticket.ConversationID) || len(ticket.EvidenceRefs) != 1) {
			return fmt.Errorf("pre-spawn execution state %q requires one acquired identity evidence ref", ticket.State)
		}
	default:
		return fmt.Errorf("invalid execution state %q", ticket.State)
	}
	return nil
}

func clonePlannedInitialInput(input *PlannedInitialInput) *PlannedInitialInput {
	if input == nil {
		return nil
	}
	cloned := *input
	return &cloned
}

func ticketInitialInputOwnsArg(ticket ExecutionTicket, index int) bool {
	return ticket.InitialInput != nil && ticket.InitialInput.Kind == InitialInputArgument &&
		ticket.InitialInput.ArgvIndex == index
}

func validateTicketInitialInput(ticket ExecutionTicket) error {
	input := ticket.InitialInput
	if input == nil {
		return nil
	}
	if input.Kind != InitialInputArgument {
		return fmt.Errorf("execution ticket initial input kind %q is unsupported", input.Kind)
	}
	if input.ArgvIndex != len(ticket.TargetArgv)-1 || input.ArgvIndex <= 0 {
		return fmt.Errorf("execution ticket initial input must be the final provider argument")
	}
	if !validDigest(input.SHA256) {
		return fmt.Errorf("execution ticket initial input digest is invalid")
	}
	sum := sha256.Sum256([]byte(ticket.TargetArgv[input.ArgvIndex]))
	if "sha256:"+hex.EncodeToString(sum[:]) != input.SHA256 {
		return fmt.Errorf("execution ticket initial input digest does not match argument")
	}
	for _, dynamic := range ticket.DynamicArgv {
		if dynamic.Index == input.ArgvIndex {
			return fmt.Errorf("execution ticket initial input cannot use a dynamic argv slot")
		}
	}
	return nil
}

func validateCodexNotifyTargetArgv(ticket ExecutionTicket) error {
	expected, err := codexNotifyOverride(PlanRequest{
		AMQExecutable: ticket.AMQExecutable,
		SessionRoot:   ticket.SessionRoot,
		Handle:        ticket.Handle,
		LaunchNonce:   ticket.LaunchNonce,
	})
	if err != nil {
		return err
	}
	count := 0
	for i := 1; i < len(ticket.TargetArgv); i++ {
		arg := ticket.TargetArgv[i]
		if arg == "--config" || strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "notify=") {
			return fmt.Errorf("codex notify override uses an unpinned configuration carrier")
		}
		if arg != "-c" {
			continue
		}
		if i+1 >= len(ticket.TargetArgv) {
			return fmt.Errorf("codex notify override does not match the execution ticket")
		}
		value := ticket.TargetArgv[i+1]
		if value == expected {
			count++
		} else if !validCodexConfigOverride(value) {
			return fmt.Errorf("codex notify execution ticket contains unsupported configuration override")
		}
		i++
	}
	if count != 1 {
		return fmt.Errorf("codex execution ticket requires exactly one notify override")
	}
	return nil
}

func WriteExecutionTicket(root *fsq.DeliveryRoot, lease *Lease, ticket ExecutionTicket) error {
	if root == nil {
		return fmt.Errorf("missing pinned session root")
	}
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if ticket.LaunchNonce != lease.LaunchNonce() {
		return fmt.Errorf("execution ticket nonce does not match launch lease")
	}
	if !lease.holdsHandle(ticket.Handle) {
		return fmt.Errorf("launch lease does not hold handle %q", ticket.Handle)
	}
	if ticket.SessionRoot != canonicalOrRaw(root.Base()) {
		return fmt.Errorf("execution ticket session root does not match pinned root")
	}
	if err := ticket.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ticket, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = root.WriteFileAtomic(executionDirectory, ticket.Handle+".json", data, 0o600)
	return err
}

func LoadExecutionTicket(root *fsq.DeliveryRoot, handle string) (ExecutionTicket, error) {
	if root == nil {
		return ExecutionTicket{}, fmt.Errorf("missing pinned session root")
	}
	if err := fsq.ValidateHandle(handle); err != nil {
		return ExecutionTicket{}, err
	}
	file, info, err := root.OpenRegularNoFollow(filepath.Join(executionDirectory, handle+".json"))
	if err != nil {
		return ExecutionTicket{}, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return ExecutionTicket{}, fmt.Errorf("execution ticket permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ExecutionTicket{}, err
	}
	var ticket ExecutionTicket
	if err := decodeExecutionJSON(data, &ticket); err != nil {
		return ExecutionTicket{}, fmt.Errorf("decode execution ticket: %w", err)
	}
	if ticket.Handle != handle {
		return ExecutionTicket{}, fmt.Errorf("execution ticket handle mismatch")
	}
	if err := ticket.Validate(); err != nil {
		return ExecutionTicket{}, err
	}
	if ticket.SessionRoot != canonicalOrRaw(root.Base()) {
		return ExecutionTicket{}, fmt.Errorf("execution ticket session root does not match pinned root")
	}
	return ticket, nil
}

// RemoveExecutionTicket removes only the exact pending generation while the
// caller still holds both the launch lease and the handle lock.
func RemoveExecutionTicket(root *fsq.DeliveryRoot, lease *Lease, handle, nonce string) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if !lease.holdsHandle(handle) {
		return fmt.Errorf("launch lease does not hold handle %q", handle)
	}
	ticket, err := LoadExecutionTicket(root, handle)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if ticket.LaunchNonce != nonce || nonce != lease.LaunchNonce() {
		return fmt.Errorf("execution ticket generation changed before removal")
	}
	return root.Remove(filepath.Join(executionDirectory, handle+".json"))
}

// CompareAndSwapExecutionTicket changes one ticket state while the caller's
// live lease and handle lock exclude competing launch/reconcile operations.
func CompareAndSwapExecutionTicket(root *fsq.DeliveryRoot, lease *Lease, handle string, expected ExecutionState, next ExecutionState, reason string) (ExecutionTicket, error) {
	if err := lease.authorizeWrite(root); err != nil {
		return ExecutionTicket{}, err
	}
	if !lease.holdsHandle(handle) {
		return ExecutionTicket{}, fmt.Errorf("launch lease does not hold handle %q", handle)
	}
	ticket, err := LoadExecutionTicket(root, handle)
	if err != nil {
		return ExecutionTicket{}, err
	}
	if ticket.LaunchNonce != lease.LaunchNonce() {
		return ExecutionTicket{}, fmt.Errorf("execution ticket nonce does not match launch lease")
	}
	if ticket.State != expected {
		return ticket, fmt.Errorf("execution ticket state is %q, want %q", ticket.State, expected)
	}
	if next != ExecutionIdentityAcquired && next != ExecutionSpawnAttempted && next != ExecutionAcknowledged && next != ExecutionPending {
		return ticket, fmt.Errorf("invalid execution state %q", next)
	}
	validTransition := expected == ExecutionPending && next == ExecutionSpawnAttempted
	validTransition = validTransition || expected == ExecutionPending && next == ExecutionIdentityAcquired
	validTransition = validTransition || expected == ExecutionIdentityAcquired && next == ExecutionSpawnAttempted
	validTransition = validTransition || expected == ExecutionSpawnAttempted && next == ExecutionAcknowledged
	validTransition = validTransition || expected == ExecutionAcknowledged && next == ExecutionPending
	validTransition = validTransition || expected == ExecutionSpawnAttempted && next == ExecutionPending
	validTransition = validTransition || expected == ExecutionSpawnAttempted && next == ExecutionIdentityAcquired
	validTransition = validTransition || expected == ExecutionAcknowledged && next == ExecutionIdentityAcquired
	if !validTransition {
		return ticket, fmt.Errorf("invalid execution state transition %q -> %q", expected, next)
	}
	ticket.State, ticket.Reason = next, reason
	if err := ticket.Validate(); err != nil {
		return ticket, err
	}
	if err := WriteExecutionTicket(root, lease, ticket); err != nil {
		return ticket, err
	}
	return ticket, nil
}

// CompareAndSwapExecutionIdentity publishes the provider-owned identity and
// its immutable evidence in the same pending-to-acquired ticket write.
func CompareAndSwapExecutionIdentity(root *fsq.DeliveryRoot, lease *Lease, handle, conversationID, evidenceRef string) (ExecutionTicket, error) {
	if err := lease.authorizeWrite(root); err != nil {
		return ExecutionTicket{}, err
	}
	if !lease.holdsHandle(handle) {
		return ExecutionTicket{}, fmt.Errorf("launch lease does not hold handle %q", handle)
	}
	ticket, err := LoadExecutionTicket(root, handle)
	if err != nil {
		return ExecutionTicket{}, err
	}
	if ticket.LaunchNonce != lease.LaunchNonce() {
		return ticket, fmt.Errorf("execution ticket nonce does not match launch lease")
	}
	if ticket.State != ExecutionPending {
		return ticket, fmt.Errorf("execution ticket state is %q, want %q", ticket.State, ExecutionPending)
	}
	ticket.ConversationID = conversationID
	ticket.EvidenceRefs = []string{evidenceRef}
	ticket.State, ticket.Reason = ExecutionIdentityAcquired, "identity_acquired"
	if err := ticket.Validate(); err != nil {
		return ticket, err
	}
	if err := validateCursorExecutionEvidence(root, ticket); err != nil {
		return ticket, err
	}
	if err := WriteExecutionTicket(root, lease, ticket); err != nil {
		return ticket, err
	}
	return ticket, nil
}

type preSpawnIdentityAcquirer interface {
	Acquire(ExecutionTicket) (CaptureEvidence, error)
}

type systemPreSpawnIdentityAcquirer struct{}

func (systemPreSpawnIdentityAcquirer) Acquire(ticket ExecutionTicket) (CaptureEvidence, error) {
	if ticket.Provider != CursorProvider || ticket.ProviderVersion != cursorCaptureVersion {
		return CaptureEvidence{}, fmt.Errorf("pre-spawn identity acquisition is unsupported for provider %q", ticket.Provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, ticket.ProviderExecutable, "create-chat")
	command.Dir = ticket.Cwd
	stdout, err := command.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return CaptureEvidence{}, fmt.Errorf("cursor create-chat timed out")
		}
		return CaptureEvidence{}, fmt.Errorf("cursor create-chat failed: %w", err)
	}
	return ParseCursorCreateChatEvidence(stdout, ticket.LaunchNonce, ticket.Handle, ticket.ProviderVersion)
}

type prepareExecutionHook func(string) error

// PrepareExecution performs the final envelope check and durable execution
// acknowledgement under the exact launch nonce and handle lock.
func PrepareExecution(root *fsq.DeliveryRoot, handle, nonce string, envelope ExecutionEnvelope) (ticket ExecutionTicket, returnErr error) {
	return prepareExecution(root, handle, nonce, envelope, systemPreSpawnIdentityAcquirer{}, nil)
}

func prepareExecution(root *fsq.DeliveryRoot, handle, nonce string, envelope ExecutionEnvelope, acquirer preSpawnIdentityAcquirer, hook prepareExecutionHook) (ticket ExecutionTicket, returnErr error) {
	lease, err := acquireExecutionLease(root, nonce)
	if err != nil {
		return ticket, err
	}
	defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
	if err := lease.LockHandles(handle); err != nil {
		return ticket, err
	}
	ticket, err = LoadExecutionTicket(root, handle)
	if err != nil {
		return ticket, err
	}
	if ticket.LaunchNonce != nonce {
		return ticket, fmt.Errorf("execution ticket nonce mismatch")
	}
	if err := ValidateExecutionEnvelope(root, ticket, envelope); err != nil {
		return ticket, err
	}
	switch ticket.Mode {
	case AdapterModeCapture:
		if ticket.PreSpawnAcquire {
			return preparePreSpawnCapture(root, lease, ticket, acquirer, hook)
		}
		return CompareAndSwapExecutionTicket(root, lease, handle, ExecutionPending, ExecutionSpawnAttempted, "spawn_attempted")
	case AdapterModeMint:
		if ticket.State == ExecutionPending {
			ticket, err = CompareAndSwapExecutionTicket(root, lease, handle, ExecutionPending, ExecutionSpawnAttempted, "spawn_attempted")
			if err != nil {
				return ticket, err
			}
		} else if ticket.State != ExecutionSpawnAttempted {
			return ticket, fmt.Errorf("execution ticket state is %q, want pending", ticket.State)
		}
		record, err := LoadConversation(root, handle)
		if err != nil {
			return ticket, err
		}
		if record.State == CapturePending {
			if record.LaunchNonce != nonce || !validUUID(ticket.ConversationID) {
				return ticket, fmt.Errorf("pending conversation does not match execution generation")
			}
			record.State = CaptureReady
			record.Identity = ConversationIdentity{Provider: ticket.Provider, ID: ticket.ConversationID}
			record.ExecutionEvidence = &ConversationExecutionEvidence{
				Backend: CommandsBackendName, Profile: CommandsProfile().Identity(), Outcome: OutcomeCreated,
				LaunchNonce: nonce, ConversationID: ticket.ConversationID,
			}
			record.Reason = ""
			if err := WriteConversation(root, lease, record); err != nil {
				return ticket, err
			}
		} else if record.State != CaptureReady || record.Identity.Provider != ticket.Provider || record.Identity.ID != ticket.ConversationID {
			return ticket, fmt.Errorf("ready conversation does not match execution target")
		}
		return CompareAndSwapExecutionTicket(root, lease, handle, ExecutionSpawnAttempted, ExecutionAcknowledged, "child_spawn_acknowledged")
	default:
		return ticket, fmt.Errorf("execution mode %q cannot acknowledge a provider process", ticket.Mode)
	}
}

func preparePreSpawnCapture(root *fsq.DeliveryRoot, lease *Lease, ticket ExecutionTicket, acquirer preSpawnIdentityAcquirer, hook prepareExecutionHook) (ExecutionTicket, error) {
	if ticket.State == ExecutionPending {
		evidence, refID, found, err := findCursorCaptureEvidence(root, ticket.Handle, ticket.LaunchNonce, ticket.ProviderVersion)
		if err != nil {
			return ticket, err
		}
		if !found {
			if acquirer == nil {
				return ticket, fmt.Errorf("cursor pre-spawn identity acquirer is unavailable")
			}
			evidence, err = acquirer.Acquire(ticket)
			if err != nil {
				return ticket, err
			}
			capture := captureCursorIdentity(CaptureRequest{
				LaunchNonce: ticket.LaunchNonce, ExpectedProviderVersion: ticket.ProviderVersion,
				Final: true, Evidence: []CaptureEvidence{evidence},
			})
			if !capture.CanPersist() || capture.Identity.Provider != ticket.Provider {
				return ticket, fmt.Errorf("cursor create-chat did not yield persistable launch evidence")
			}
			refs, persistErr := persistProviderCaptureEvidenceWithContext(root, lease, ticket.Handle, ticket.CallerContext, []CaptureEvidence{evidence})
			if persistErr != nil {
				return ticket, persistErr
			}
			refID = refs[0]
		}
		if err := callPrepareExecutionHook(hook, "evidence_persisted"); err != nil {
			return ticket, err
		}
		ticket, err = CompareAndSwapExecutionIdentity(root, lease, ticket.Handle, evidence.conversationID, refID)
		if err != nil {
			return ticket, err
		}
		if err := callPrepareExecutionHook(hook, "identity_acquired"); err != nil {
			return ticket, err
		}
	}
	if ticket.State == ExecutionIdentityAcquired || ticket.State == ExecutionSpawnAttempted || ticket.State == ExecutionAcknowledged {
		if err := validateCursorExecutionEvidence(root, ticket); err != nil {
			return ticket, err
		}
	}
	if ticket.State == ExecutionIdentityAcquired {
		var err error
		ticket, err = CompareAndSwapExecutionTicket(root, lease, ticket.Handle, ExecutionIdentityAcquired, ExecutionSpawnAttempted, "spawn_attempted")
		if err != nil {
			return ticket, err
		}
		if err := callPrepareExecutionHook(hook, "spawn_attempted"); err != nil {
			return ticket, err
		}
	}
	if ticket.State == ExecutionSpawnAttempted {
		if err := promotePreSpawnConversation(root, lease, ticket); err != nil {
			return ticket, err
		}
		var err error
		ticket, err = CompareAndSwapExecutionTicket(root, lease, ticket.Handle, ExecutionSpawnAttempted, ExecutionAcknowledged, "child_spawn_acknowledged")
		if err != nil {
			return ticket, err
		}
	}
	if ticket.State != ExecutionAcknowledged {
		return ticket, fmt.Errorf("execution ticket state is %q, want acknowledged", ticket.State)
	}
	return ticket, nil
}

func promotePreSpawnConversation(root *fsq.DeliveryRoot, lease *Lease, ticket ExecutionTicket) error {
	record, err := LoadConversation(root, ticket.Handle)
	if err != nil {
		return err
	}
	if record.State == CaptureReady {
		if record.Identity.Provider != ticket.Provider || record.Identity.ID != ticket.ConversationID ||
			record.LaunchNonce != ticket.LaunchNonce || !reflect.DeepEqual(record.EvidenceRefs, ticket.EvidenceRefs) ||
			record.ExecutionEvidence == nil || record.ExecutionEvidence.Backend != ticket.Backend ||
			record.ExecutionEvidence.Profile != ticket.Profile || record.ExecutionEvidence.LaunchNonce != ticket.LaunchNonce ||
			record.ExecutionEvidence.ConversationID != ticket.ConversationID {
			return fmt.Errorf("ready conversation does not match acquired provider identity")
		}
		return nil
	}
	if record.State != CapturePending || record.LaunchNonce != ticket.LaunchNonce {
		return fmt.Errorf("pending conversation does not match acquired provider generation")
	}
	record.State = CaptureReady
	record.Identity = ConversationIdentity{Provider: ticket.Provider, ID: ticket.ConversationID}
	record.EvidenceRefs = append([]string(nil), ticket.EvidenceRefs...)
	record.ExecutionEvidence = &ConversationExecutionEvidence{
		Backend: ticket.Backend, Profile: ticket.Profile, Outcome: OutcomeCreated,
		LaunchNonce: ticket.LaunchNonce, ConversationID: ticket.ConversationID,
	}
	record.Reason = ""
	return WriteConversation(root, lease, record)
}

func validateCursorExecutionEvidence(root *fsq.DeliveryRoot, ticket ExecutionTicket) error {
	if len(ticket.EvidenceRefs) != 1 {
		return fmt.Errorf("cursor execution ticket requires one evidence ref")
	}
	record, _, err := ReadEvidence(root, ticket.EvidenceRefs[0])
	if err != nil {
		return err
	}
	payload, err := decodeCursorCreateChatPayload(record.Payload)
	if err != nil {
		return err
	}
	if record.Kind != EvidenceProviderCapture || record.Handle != ticket.Handle || payload.Handle != ticket.Handle ||
		payload.Provider != ticket.Provider || payload.LaunchNonce != ticket.LaunchNonce ||
		payload.ProviderVersion != ticket.ProviderVersion || payload.ConversationID != ticket.ConversationID ||
		!reflect.DeepEqual(record.CallerContext, ticket.CallerContext) {
		return fmt.Errorf("cursor execution evidence binding mismatch")
	}
	return nil
}

type CodexNotifyConflictError struct {
	Existing string
	Observed string
}

func (err *CodexNotifyConflictError) Error() string {
	return fmt.Sprintf("codex_notify_identity_conflict: existing %s, observed %s", err.Existing, err.Observed)
}

type CodexNotifyResult struct {
	ConversationID string
	EvidenceRef    string
	AlreadyReady   bool
}

// RecordCodexNotify binds one provider-owned turn-complete notification to
// the exact managed launch, persists immutable evidence, and only then makes
// the provider identity resumable.
func RecordCodexNotify(root *fsq.DeliveryRoot, handle, nonce, amqExecutable string, raw []byte) (CodexNotifyResult, error) {
	return recordCodexNotify(root, handle, nonce, amqExecutable, raw, nil)
}

func recordCodexNotify(root *fsq.DeliveryRoot, handle, nonce, amqExecutable string, raw []byte, hook prepareExecutionHook) (result CodexNotifyResult, returnErr error) {
	lease, err := AcquireLease(root, nonce)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
	if err := lease.LockHandles(handle); err != nil {
		return result, err
	}
	ticket, err := LoadExecutionTicket(root, handle)
	if err != nil {
		return result, err
	}
	if ticket.LaunchNonce != nonce || ticket.Handle != handle || ticket.Mode != AdapterModeCapture ||
		ticket.Provider != CodexProvider || ticket.ProviderVersion != codexCaptureVersion || ticket.PreSpawnAcquire {
		return result, fmt.Errorf("codex notify execution ticket binding mismatch")
	}
	if ticket.State != ExecutionSpawnAttempted && ticket.State != ExecutionAcknowledged {
		return result, fmt.Errorf("codex notify execution ticket state is %q", ticket.State)
	}
	_, amqIdentity, err := canonicalFile(amqExecutable)
	if err != nil || amqIdentity != ticket.AMQExecutableIdentity {
		return result, fmt.Errorf("codex notify AMQ executable identity mismatch")
	}
	evidence, err := ParseCodexNotifyEvidence(raw, nonce, handle, ticket.ProviderVersion, ticket.Cwd)
	if err != nil {
		return result, err
	}
	record, err := LoadConversation(root, handle)
	if err != nil {
		return result, err
	}
	if record.State == CaptureReady {
		if record.LaunchNonce != nonce || record.ProviderVersion != ticket.ProviderVersion ||
			record.Identity.Provider != CodexProvider || record.Identity.ID != evidence.conversationID {
			return result, &CodexNotifyConflictError{Existing: record.Identity.ID, Observed: evidence.conversationID}
		}
		if len(record.EvidenceRefs) != 1 {
			return result, fmt.Errorf("ready codex conversation does not have one evidence ref")
		}
		return CodexNotifyResult{ConversationID: record.Identity.ID, EvidenceRef: record.EvidenceRefs[0], AlreadyReady: true}, nil
	}
	if record.State != CapturePending || record.LaunchNonce != nonce || record.ProviderVersion != ticket.ProviderVersion {
		return result, fmt.Errorf("pending codex conversation does not match notify generation")
	}

	stored, refID, found, err := findProviderCaptureEvidence(root, CodexProvider, handle, nonce, ticket.ProviderVersion)
	if err != nil {
		return result, err
	}
	if found {
		_, ref, readErr := ReadEvidence(root, refID)
		if readErr != nil || !reflect.DeepEqual(ref.CallerContext, ticket.CallerContext) {
			return result, fmt.Errorf("codex notify evidence caller context mismatch")
		}
		if stored.conversationID != evidence.conversationID {
			return result, &CodexNotifyConflictError{Existing: stored.conversationID, Observed: evidence.conversationID}
		}
	} else {
		refs, persistErr := persistProviderCaptureEvidenceWithContext(root, lease, handle, ticket.CallerContext, []CaptureEvidence{evidence})
		if persistErr != nil {
			return result, persistErr
		}
		refID = refs[0]
	}
	if err := callPrepareExecutionHook(hook, "evidence_persisted"); err != nil {
		return result, err
	}
	record.State = CaptureReady
	record.Identity = ConversationIdentity{Provider: CodexProvider, ID: evidence.conversationID}
	record.EvidenceRefs = []string{refID}
	record.ExecutionEvidence = &ConversationExecutionEvidence{
		Backend: ticket.Backend, Profile: ticket.Profile, Outcome: OutcomeCreated,
		LaunchNonce: nonce, ConversationID: evidence.conversationID,
	}
	record.Reason = ""
	if err := WriteConversation(root, lease, record); err != nil {
		return result, err
	}
	if err := callPrepareExecutionHook(hook, "conversation_promoted"); err != nil {
		return result, err
	}
	return CodexNotifyResult{ConversationID: evidence.conversationID, EvidenceRef: refID}, nil
}

func callPrepareExecutionHook(hook prepareExecutionHook, stage string) error {
	if hook == nil {
		return nil
	}
	return hook(stage)
}

// ResolveExecutionArgv replaces only declared dynamic slots after the ticket
// has durably acquired their values. Static argv remains byte-identical.
func ResolveExecutionArgv(ticket ExecutionTicket) ([]string, error) {
	if err := ticket.Validate(); err != nil {
		return nil, err
	}
	if ticket.PreSpawnAcquire && ticket.State != ExecutionAcknowledged {
		return nil, fmt.Errorf("pre-spawn execution identity is not acknowledged")
	}
	argv := append([]string(nil), ticket.TargetArgv...)
	for _, dynamic := range ticket.DynamicArgv {
		switch dynamic.Kind {
		case DynamicArgLaunchNonce:
			argv[dynamic.Index] = ticket.LaunchNonce
		case DynamicArgConversationID:
			if !validUUID(ticket.ConversationID) {
				return nil, fmt.Errorf("conversation identity is not acquired")
			}
			argv[dynamic.Index] = ticket.ConversationID
		default:
			return nil, fmt.Errorf("unsupported dynamic argv kind %q", dynamic.Kind)
		}
	}
	return argv, nil
}

func acquireExecutionLease(root *fsq.DeliveryRoot, nonce string) (*Lease, error) {
	deadline := time.Now().Add(executionLeaseWaitTimeout)
	for {
		lease, err := AcquireLease(root, nonce)
		if err == nil {
			return lease, nil
		}
		var held *LeaseHeldError
		if !errors.As(err, &held) || held.Nonce != nonce {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("wait for own launch lease: %w", err)
		}
		delay := executionLeaseRetryInterval
		if remaining < delay {
			delay = remaining
		}
		time.Sleep(delay)
	}
}

// RevertExecution records a provider exec failure. It demotes only a ready
// mint generation created by this exact ticket; an older resumed identity is
// retained.
func RevertExecution(root *fsq.DeliveryRoot, handle, nonce string) (returnErr error) {
	lease, err := AcquireLease(root, nonce)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
	if err := lease.LockHandles(handle); err != nil {
		return err
	}
	ticket, err := LoadExecutionTicket(root, handle)
	if err != nil || ticket.LaunchNonce != nonce {
		if err != nil {
			return err
		}
		return fmt.Errorf("execution ticket nonce mismatch")
	}
	if ticket.Mode == AdapterModeMint {
		record, loadErr := LoadConversation(root, handle)
		if loadErr != nil {
			return loadErr
		}
		if record.State == CaptureReady && record.LaunchNonce == nonce && record.ExecutionEvidence != nil && record.ExecutionEvidence.LaunchNonce == nonce {
			record.State, record.Identity, record.ExecutionEvidence, record.EvidenceRefs = CapturePending, ConversationIdentity{}, nil, nil
			record.Reason = CaptureReason("spawn_failed")
			if err := WriteConversation(root, lease, record); err != nil {
				return err
			}
		}
		_, err = CompareAndSwapExecutionTicket(root, lease, handle, ExecutionAcknowledged, ExecutionPending, "spawn_failed")
		return err
	}
	if ticket.Mode == AdapterModeCapture {
		if ticket.PreSpawnAcquire {
			if ticket.State == ExecutionIdentityAcquired {
				return nil
			}
			_, err = CompareAndSwapExecutionTicket(root, lease, handle, ticket.State, ExecutionIdentityAcquired, "spawn_failed")
			return err
		}
		_, err = CompareAndSwapExecutionTicket(root, lease, handle, ExecutionSpawnAttempted, ExecutionPending, "spawn_failed")
		return err
	}
	return fmt.Errorf("execution mode %q cannot be reverted", ticket.Mode)
}

func ValidateExecutionEnvelope(root *fsq.DeliveryRoot, ticket ExecutionTicket, envelope ExecutionEnvelope) error {
	if root == nil {
		return fmt.Errorf("missing pinned session root")
	}
	if err := root.VerifyBase(); err != nil {
		return err
	}
	session, sessionID, err := canonicalDirectory(root.Base())
	if err != nil || session != ticket.SessionRoot || sessionID != ticket.SessionIdentity {
		return fmt.Errorf("session root identity changed")
	}
	project, projectID, err := canonicalDirectory(ticket.ProjectRoot)
	if err != nil || project != ticket.ProjectRoot || projectID != ticket.ProjectIdentity {
		return fmt.Errorf("project identity changed")
	}
	cwd, cwdID, err := canonicalDirectory(envelope.Cwd)
	if err != nil || cwd != ticket.Cwd || cwdID != ticket.CwdIdentity {
		return fmt.Errorf("working directory identity changed")
	}
	provider, providerID, err := canonicalFile(envelope.ProviderExecutable)
	if err != nil || provider != ticket.ProviderExecutable || providerID != ticket.ProviderExecutableIdentity || pathWithin(provider, project) {
		return fmt.Errorf("provider executable identity changed")
	}
	amq, amqID, err := canonicalFile(envelope.AMQExecutable)
	if err != nil || amq != ticket.AMQExecutable || amqID != ticket.AMQExecutableIdentity {
		return fmt.Errorf("amq executable identity changed")
	}
	if !reflect.DeepEqual(envelope.TargetArgv, ticket.TargetArgv) {
		return fmt.Errorf("provider argv changed")
	}
	if err := validateExecutionOptions(ticket, envelope.Execution); err != nil {
		return err
	}
	actualEnv := make(map[string]string, len(envelope.Environment))
	for _, item := range envelope.Environment {
		if key, value, ok := strings.Cut(item, "="); ok {
			actualEnv[key] = value
		}
	}
	for key, value := range ticket.TargetEnv {
		if actualEnv[key] != value {
			return fmt.Errorf("provider environment %q changed", key)
		}
	}
	return nil
}

// ValidateExecutionOptions performs the read-only ticket check required
// before coop exec can use wake or injector settings. PrepareExecution repeats
// the same check at the final provider boundary before changing ticket state.
func ValidateExecutionOptions(root *fsq.DeliveryRoot, handle, nonce string, options *PrepareExecutionOptions) error {
	if root == nil {
		return fmt.Errorf("missing pinned session root")
	}
	if err := root.VerifyBase(); err != nil {
		return err
	}
	ticket, err := LoadExecutionTicket(root, handle)
	if err != nil {
		return err
	}
	if ticket.LaunchNonce != nonce {
		return fmt.Errorf("execution ticket nonce changed")
	}
	session, sessionID, err := canonicalDirectory(root.Base())
	if err != nil || session != ticket.SessionRoot || sessionID != ticket.SessionIdentity {
		return fmt.Errorf("session root identity changed")
	}
	return validateExecutionOptions(ticket, options)
}

func validateExecutionOptions(ticket ExecutionTicket, options *PrepareExecutionOptions) error {
	if !reflect.DeepEqual(CanonicalExecutionOptions(options), CanonicalExecutionOptions(ticket.Execution)) {
		return fmt.Errorf("execution options changed")
	}
	if ticket.Execution != nil && ticket.Execution.InjectorVia != "" {
		injector, injectorID, err := canonicalFile(ticket.Execution.InjectorVia)
		if err != nil || injector != ticket.InjectorExecutable || injectorID != ticket.InjectorExecutableIdentity {
			return fmt.Errorf("injector executable identity changed")
		}
	}
	return nil
}

// CanonicalExecutionOptions returns the normalized policy value used for
// ticket equality. A missing policy and an explicit all-default policy are
// equivalent; collection presence is not semantic.
func CanonicalExecutionOptions(options *PrepareExecutionOptions) PrepareExecutionOptions {
	var canonical PrepareExecutionOptions
	if options != nil {
		canonical = *clonePrepareExecutionOptions(options)
	}
	if canonical.WakeMode == "" {
		canonical.WakeMode = "enabled"
	}
	if canonical.InjectorMode == "" {
		canonical.InjectorMode = "none"
	}
	if len(canonical.InjectorArgs) == 0 {
		canonical.InjectorArgs = nil
	}
	if len(canonical.SymphonyEvents) == 0 {
		canonical.SymphonyEvents = nil
	}
	return canonical
}

func canonicalDirectory(path string) (string, string, error) { return canonicalPath(path, true) }
func canonicalFile(path string) (string, string, error)      { return canonicalPath(path, false) }
func canonicalPath(path string, directory bool) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if !directory && !filepath.IsAbs(path) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", "", err
		}
		path = resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", err
	}
	if directory != info.IsDir() {
		return "", "", fmt.Errorf("path has wrong type")
	}
	identity, err := executionPhysicalIdentity(resolved, info)
	if err != nil {
		return "", "", err
	}
	return filepath.Clean(resolved), identity, nil
}

func executionPhysicalIdentity(path string, info os.FileInfo) (string, error) {
	if info == nil || info.Sys() == nil {
		return "", fmt.Errorf("physical identity unavailable for %s", path)
	}
	if info.IsDir() {
		return fsq.StableTreeIdentityInfo(info)
	}
	return fsq.StableFileIdentity(path)
}

func executionEnvDigest(env map[string]string) (string, error) {
	data, err := json.Marshal(cloneExecutionEnv(env))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func cloneExecutionEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
func decodeExecutionJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
func canonicalOrRaw(path string) string {
	value, err := filepath.EvalSymlinks(path)
	if err != nil {
		value = path
	}
	abs, err := filepath.Abs(value)
	if err == nil {
		value = abs
	}
	return filepath.Clean(value)
}
