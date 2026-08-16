package launch

import (
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
	ExecutionPending        ExecutionState = "pending"
	ExecutionSpawnAttempted ExecutionState = "spawn_attempted"
	ExecutionAcknowledged   ExecutionState = "acknowledged"
)

// ExecutionTicket is the durable, nonce-bound handoff between planning and
// the process which actually starts a command. It is evidence, not execution
// authority: writes require the live launch lease and the matching handle lock.
type ExecutionTicket struct {
	Version int `json:"version"`

	Handle         string      `json:"handle"`
	LaunchNonce    string      `json:"launch_nonce"`
	Mode           AdapterMode `json:"mode"`
	Provider       string      `json:"provider"`
	ConversationID string      `json:"conversation_id,omitempty"`

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

	TargetArgv []string                 `json:"target_argv"`
	TargetEnv  map[string]string        `json:"target_env,omitempty"`
	EnvDigest  string                   `json:"env_digest"`
	State      ExecutionState           `json:"state"`
	Reason     string                   `json:"reason,omitempty"`
	Execution  *PrepareExecutionOptions `json:"execution,omitempty"`
}

type ExecutionTicketRequest struct {
	Handle, LaunchNonce               string
	Mode                              AdapterMode
	Provider, ConversationID          string
	ProjectRoot, SessionRoot, Cwd     string
	ProviderExecutable, AMQExecutable string
	TargetArgv                        []string
	TargetEnv                         map[string]string
	State                             ExecutionState
	Reason                            string
	Execution                         *PrepareExecutionOptions
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
		Provider: request.Provider, ConversationID: request.ConversationID,
		ProjectRoot: project, ProjectIdentity: projectID, SessionRoot: session, SessionIdentity: sessionID,
		Cwd: cwd, CwdIdentity: cwdID, ProviderExecutable: provider, ProviderExecutableIdentity: providerID,
		AMQExecutable: amq, AMQExecutableIdentity: amqID, TargetArgv: append([]string(nil), request.TargetArgv...),
		TargetEnv: env, EnvDigest: digest, State: request.State, Reason: request.Reason,
		Execution: execution, InjectorExecutable: injector, InjectorExecutableIdentity: injectorID,
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
		if arg == "" || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("target argv[%d] is invalid", i)
		}
	}
	if digest, err := executionEnvDigest(ticket.TargetEnv); err != nil || digest != ticket.EnvDigest {
		if err != nil {
			return err
		}
		return fmt.Errorf("environment digest does not match target environment")
	}
	switch ticket.State {
	case ExecutionPending:
	case ExecutionSpawnAttempted, ExecutionAcknowledged:
		if strings.TrimSpace(ticket.Reason) == "" {
			return fmt.Errorf("reason is required for execution state %q", ticket.State)
		}
	default:
		return fmt.Errorf("invalid execution state %q", ticket.State)
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
	if next != ExecutionSpawnAttempted && next != ExecutionAcknowledged && next != ExecutionPending {
		return ticket, fmt.Errorf("invalid execution state %q", next)
	}
	validTransition := expected == ExecutionPending && next == ExecutionSpawnAttempted
	validTransition = validTransition || expected == ExecutionSpawnAttempted && next == ExecutionAcknowledged
	validTransition = validTransition || expected == ExecutionAcknowledged && next == ExecutionPending
	validTransition = validTransition || expected == ExecutionSpawnAttempted && next == ExecutionPending
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

// PrepareExecution performs the final envelope check and durable execution
// acknowledgement under the exact launch nonce and handle lock.
func PrepareExecution(root *fsq.DeliveryRoot, handle, nonce string, envelope ExecutionEnvelope) (ticket ExecutionTicket, returnErr error) {
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
			record.State, record.Identity, record.ExecutionEvidence = CapturePending, ConversationIdentity{}, nil
			record.Reason = CaptureReason("spawn_failed")
			if err := WriteConversation(root, lease, record); err != nil {
				return err
			}
		}
		_, err = CompareAndSwapExecutionTicket(root, lease, handle, ExecutionAcknowledged, ExecutionPending, "spawn_failed")
		return err
	}
	if ticket.Mode == AdapterModeCapture {
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
