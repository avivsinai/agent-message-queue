// Package launch defines the versioned contracts shared by launch
// orchestration, harness adapters, and terminal backends.
package launch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const PlanVersion = 1

const preSpawnConversationPlaceholder = "__AMQ_CONVERSATION_ID__"

type AdapterMode string

const (
	AdapterModeMint        AdapterMode = "mint"
	AdapterModeCapture     AdapterMode = "capture"
	AdapterModeUnsupported AdapterMode = "unsupported"
)

type ResumePolicy string

const (
	ResumeEnabled  ResumePolicy = "resume"
	ResumeFresh    ResumePolicy = "fresh"
	ResumeDisabled ResumePolicy = "disabled"
)

type DynamicArgKind string

const (
	DynamicArgLaunchNonce    DynamicArgKind = "launch_nonce"
	DynamicArgConversationID DynamicArgKind = "conversation_id"
)

type InitialInputKind string

const (
	InitialInputArgument InitialInputKind = "argument"
	InitialInputStdin    InitialInputKind = "stdin"
	InitialInputFile     InitialInputKind = "file"
)

type PlannedInitialInput struct {
	Kind      InitialInputKind `json:"kind"`
	SHA256    string           `json:"sha256"`
	ArgvIndex int              `json:"argv_index"`
}

// Plan is the public, backend-independent execution contract. Nonce and
// ConversationID are per-launch values and are deliberately not trusted.
type Plan struct {
	Version int         `json:"version"`
	Agents  []AgentPlan `json:"agents"`
}

type AgentPlan struct {
	Handle          string            `json:"handle"`
	Argv            []string          `json:"argv"`
	EnvOverlay      map[string]string `json:"env_overlay,omitempty"`
	Cwd             string            `json:"cwd"`
	AdapterMode     AdapterMode       `json:"adapter_mode"`
	ResumePolicy    ResumePolicy      `json:"resume_policy"`
	LaunchNonce     string            `json:"launch_nonce,omitempty"`
	ConversationID  string            `json:"conversation_id,omitempty"`
	DynamicArgv     []DynamicArg      `json:"dynamic_argv,omitempty"`
	PreSpawnAcquire bool              `json:"pre_spawn_acquire,omitempty"`
	// Execution is the normalized coop-exec wrapper policy. Keeping it in the
	// plan makes journal recovery lossless before the wrapper consumes it.
	Execution    *PrepareExecutionOptions `json:"execution,omitempty"`
	InitialInput *PlannedInitialInput     `json:"initial_input,omitempty"`
}

// DynamicArg marks one runtime-generated argv value. Unmarked argv values are
// always trust-bearing. Version 1 has no dynamic environment slots; adding
// them requires a plan schema version change when an adapter needs one.
type DynamicArg struct {
	Index int            `json:"index"`
	Kind  DynamicArgKind `json:"kind"`
}

func (p Plan) Validate() error {
	if p.Version != PlanVersion {
		return fmt.Errorf("unsupported launch plan version %d", p.Version)
	}
	if len(p.Agents) == 0 {
		return fmt.Errorf("launch plan has no agents")
	}
	seen := make(map[string]struct{}, len(p.Agents))
	for i, agent := range p.Agents {
		if err := agent.validate(); err != nil {
			return fmt.Errorf("agents[%d]: %w", i, err)
		}
		if _, ok := seen[agent.Handle]; ok {
			return fmt.Errorf("duplicate agent handle %q", agent.Handle)
		}
		seen[agent.Handle] = struct{}{}
	}
	return nil
}

func (a AgentPlan) validate() error {
	if err := fsq.ValidateHandle(a.Handle); err != nil {
		return fmt.Errorf("invalid handle: %w", err)
	}
	if len(a.Argv) == 0 || strings.TrimSpace(a.Argv[0]) == "" {
		return fmt.Errorf("argv must name an executable")
	}
	if strings.TrimSpace(a.Cwd) == "" {
		return fmt.Errorf("cwd is required")
	}
	switch a.AdapterMode {
	case AdapterModeMint, AdapterModeCapture, AdapterModeUnsupported:
	default:
		return fmt.Errorf("invalid adapter mode %q", a.AdapterMode)
	}
	switch a.ResumePolicy {
	case ResumeEnabled, ResumeFresh, ResumeDisabled:
	default:
		return fmt.Errorf("invalid resume policy %q", a.ResumePolicy)
	}
	for key := range a.EnvOverlay {
		if key == "" || strings.ContainsRune(key, '=') {
			return fmt.Errorf("invalid environment key %q", key)
		}
	}
	if a.PreSpawnAcquire && (a.AdapterMode != AdapterModeCapture || a.ConversationID != "") {
		return fmt.Errorf("pre-spawn acquisition requires capture mode without a planned identity")
	}
	seenDynamic := make(map[int]struct{}, len(a.DynamicArgv))
	conversationSlots := 0
	for i, dynamic := range a.DynamicArgv {
		if dynamic.Index < 0 || dynamic.Index >= len(a.Argv) {
			return fmt.Errorf("dynamic_argv[%d]: index %d is outside argv", i, dynamic.Index)
		}
		if dynamic.Index == 0 {
			return fmt.Errorf("dynamic_argv[%d]: argv[0] executable must be static", i)
		}
		if _, ok := seenDynamic[dynamic.Index]; ok {
			return fmt.Errorf("dynamic_argv[%d]: duplicate index %d", i, dynamic.Index)
		}
		seenDynamic[dynamic.Index] = struct{}{}
		var expected string
		switch dynamic.Kind {
		case DynamicArgLaunchNonce:
			expected = a.LaunchNonce
		case DynamicArgConversationID:
			conversationSlots++
			expected = a.ConversationID
		default:
			return fmt.Errorf("dynamic_argv[%d]: invalid kind %q", i, dynamic.Kind)
		}
		if dynamic.Kind == DynamicArgConversationID && a.PreSpawnAcquire && a.AdapterMode == AdapterModeCapture &&
			a.ConversationID == "" && a.Argv[dynamic.Index] == preSpawnConversationPlaceholder {
			continue
		}
		if expected == "" || a.Argv[dynamic.Index] != expected {
			return fmt.Errorf("dynamic_argv[%d]: argv value does not match %s", i, dynamic.Kind)
		}
	}
	if a.PreSpawnAcquire && conversationSlots != 1 {
		return fmt.Errorf("pre-spawn acquisition requires exactly one dynamic conversation slot")
	}
	if a.InitialInput != nil {
		if a.InitialInput.Kind != InitialInputArgument {
			return fmt.Errorf("invalid initial input kind %q", a.InitialInput.Kind)
		}
		if !validDigest(a.InitialInput.SHA256) {
			return fmt.Errorf("initial input digest is invalid")
		}
		if a.InitialInput.ArgvIndex <= 0 || a.InitialInput.ArgvIndex >= len(a.Argv) {
			return fmt.Errorf("initial input argv index is invalid")
		}
		if _, dynamic := seenDynamic[a.InitialInput.ArgvIndex]; dynamic {
			return fmt.Errorf("initial input argv index is dynamic")
		}
		sum := sha256.Sum256([]byte(a.Argv[a.InitialInput.ArgvIndex]))
		if "sha256:"+hex.EncodeToString(sum[:]) != a.InitialInput.SHA256 {
			return fmt.Errorf("initial input digest does not match argument")
		}
	}
	return nil
}

// Validate checks one backend-ready agent plan independently of its parent.
func (a AgentPlan) Validate() error {
	return a.validate()
}

type canonicalArgument struct {
	Value   string         `json:"value,omitempty"`
	Dynamic DynamicArgKind `json:"dynamic,omitempty"`
}

type staticAgentPlan struct {
	Handle          string                 `json:"handle"`
	Argv            []canonicalArgument    `json:"argv"`
	EnvOverlay      map[string]string      `json:"env_overlay,omitempty"`
	Cwd             string                 `json:"cwd"`
	AdapterMode     AdapterMode            `json:"adapter_mode"`
	ResumePolicy    ResumePolicy           `json:"resume_policy"`
	PreSpawnAcquire bool                   `json:"pre_spawn_acquire,omitempty"`
	InitialInput    *canonicalInitialInput `json:"initial_input,omitempty"`
}

type canonicalInitialInput struct {
	Kind      InitialInputKind `json:"kind"`
	SHA256    string           `json:"sha256,omitempty"`
	ArgvIndex int              `json:"argv_index"`
}

// SemanticDigest hashes the adapter-normalized static execution template.
// Fresh and resume argv shapes have different digests because only the resume
// shape carries a conversation slot; each shape therefore requires trust once.
func (p Plan) SemanticDigest() (string, error) {
	return p.semanticDigest(false, false)
}

// TrustSemanticDigest excludes caller-generated initial-input content while
// retaining the selected carrier kind and argv position.
func (p Plan) TrustSemanticDigest() (string, error) {
	return p.semanticDigest(false, true)
}

// PreparePlanDigest hashes the nonce-free static plan used by Prepare. Unlike
// a backend execution Plan, it permits zero runnable agents so a participant-
// only session still has one deterministic plan subject.
func PreparePlanDigest(p Plan) (string, error) {
	return p.semanticDigest(true, false)
}

func PrepareTrustPlanDigest(p Plan) (string, error) {
	return p.semanticDigest(true, true)
}

func (p Plan) semanticDigest(allowEmpty, trustProjection bool) (string, error) {
	if err := p.validateForDigest(allowEmpty); err != nil {
		return "", err
	}
	agents := make([]staticAgentPlan, len(p.Agents))
	for i, agent := range p.Agents {
		argv := make([]canonicalArgument, len(agent.Argv))
		for j, value := range agent.Argv {
			argv[j].Value = value
		}
		for _, dynamic := range agent.DynamicArgv {
			argv[dynamic.Index] = canonicalArgument{Dynamic: dynamic.Kind}
		}
		var initial *canonicalInitialInput
		if agent.InitialInput != nil {
			initial = &canonicalInitialInput{Kind: agent.InitialInput.Kind, SHA256: agent.InitialInput.SHA256, ArgvIndex: agent.InitialInput.ArgvIndex}
			if trustProjection {
				argv[agent.InitialInput.ArgvIndex] = canonicalArgument{Value: "${initial_input:" + string(agent.InitialInput.Kind) + "}"}
				initial.SHA256 = ""
			}
		}
		agents[i] = staticAgentPlan{
			Handle: agent.Handle, Argv: argv, EnvOverlay: agent.EnvOverlay,
			Cwd: agent.Cwd, AdapterMode: agent.AdapterMode, ResumePolicy: agent.ResumePolicy,
			PreSpawnAcquire: agent.PreSpawnAcquire, InitialInput: initial,
		}
	}
	slices.SortFunc(agents, func(a, b staticAgentPlan) int { return strings.Compare(a.Handle, b.Handle) })
	canonical, err := json.Marshal(struct {
		Version int               `json:"version"`
		Agents  []staticAgentPlan `json:"agents"`
	}{Version: p.Version, Agents: agents})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (p Plan) validateForDigest(allowEmpty bool) error {
	if !allowEmpty || len(p.Agents) != 0 {
		return p.Validate()
	}
	if p.Version != PlanVersion {
		return fmt.Errorf("unsupported launch plan version %d", p.Version)
	}
	return nil
}

func DecodePlan(data []byte) (Plan, error) {
	var plan Plan
	if err := decodeStrict(data, &plan); err != nil {
		return Plan{}, fmt.Errorf("decode launch plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
