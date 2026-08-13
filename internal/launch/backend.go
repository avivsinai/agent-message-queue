package launch

import (
	"fmt"
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Backend is the launcher contract from #480 v1.1 §6.
// Attach-or-recreate is an orchestration decision from Inspect evidence,
// never a backend method.
type Backend interface {
	Detect() DetectResult
	Create(CreateRequest) (CreateResult, error)
	Inspect(InspectRequest) (InspectResult, error)
	Close(CloseRequest) (CloseResult, error)
}

// BackendFocuser is the optional managed attach surface. It stays separate
// from the four-method backend floor: plan_only backends do not own resources,
// while a managed profile that declares CapFocus must implement this seam.
type BackendFocuser interface {
	Focus(FocusRequest) (FocusResult, error)
}

type Capability string

const (
	// CapPlanOnly is the commands-backend floor: emit an executable plan,
	// never claim a managed terminal resource.
	CapPlanOnly Capability = "plan_only"
	// CapCreate is managed layout creation (writes a binding).
	CapCreate Capability = "create"
	// CapInspect means the backend can distinguish present from absent.
	CapInspect Capability = "inspect"
	// CapClose means the backend can dispose of a resource it owns.
	CapClose Capability = "close"
	// CapFocus means the backend can attach to a present layout.
	CapFocus Capability = "focus"
)

type Outcome string

const (
	OutcomeCreated         Outcome = "created"
	OutcomeCommandsEmitted Outcome = "commands_emitted"
	OutcomeActionRequired  Outcome = "action_required"
	OutcomeUnsupported     Outcome = "unsupported"
	OutcomeAttached        Outcome = "attached"
)

type InspectStatus string

const (
	InspectPresent InspectStatus = "present"
	InspectAbsent  InspectStatus = "absent"
	InspectUnknown InspectStatus = "unknown"
)

// Profile is the versioned static maximum envelope for one
// (backend, platform, version-range). Conformance graduates this identity;
// Detect reports the runtime subset separately so the envelope cannot shrink
// to dodge a failing test.
type Profile struct {
	Backend      string       `json:"backend"`
	Platform     string       `json:"platform"`
	VersionRange string       `json:"version_range"`
	Version      int          `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

func (p Profile) Identity() string {
	return fmt.Sprintf("%s/%s/v%d", p.Backend, p.Platform, p.Version)
}

func (p Profile) Has(c Capability) bool {
	return slices.Contains(p.Capabilities, c)
}

type Degradation struct {
	Capability Capability `json:"capability"`
	Reason     string     `json:"reason"`
}

type DetectResult struct {
	Available        bool          `json:"available"`
	Profile          Profile       `json:"profile"`
	HostIdentity     string        `json:"host_identity,omitempty"`
	InstanceIdentity string        `json:"instance_identity,omitempty"`
	Effective        []Capability  `json:"effective"`
	Degradations     []Degradation `json:"degradations,omitempty"`
}

func (d DetectResult) Validate() error {
	if d.Profile.Backend == "" || d.Profile.Platform == "" || d.Profile.Version < 1 {
		return fmt.Errorf("capability profile is incomplete")
	}
	if d.Profile.Identity() == "" {
		return fmt.Errorf("capability profile identity is empty")
	}
	seen := make(map[Capability]struct{}, len(d.Profile.Capabilities))
	for _, c := range d.Profile.Capabilities {
		if c == "" {
			return fmt.Errorf("empty capability in static profile")
		}
		if _, ok := seen[c]; ok {
			return fmt.Errorf("duplicate capability %q", c)
		}
		seen[c] = struct{}{}
	}
	for _, c := range d.Effective {
		if !d.Profile.Has(c) {
			return fmt.Errorf("effective capability %q is outside the static profile", c)
		}
	}
	for _, deg := range d.Degradations {
		if !d.Profile.Has(deg.Capability) {
			return fmt.Errorf("degradation %q is outside the static profile", deg.Capability)
		}
		if slices.Contains(d.Effective, deg.Capability) {
			return fmt.Errorf("degradation %q is still in the effective set", deg.Capability)
		}
		if deg.Reason == "" {
			return fmt.Errorf("degradation %q has no reason", deg.Capability)
		}
	}
	return nil
}

type CreateRequest struct {
	Session string
	Plan    Plan
	AMQPath string
	Root    *fsq.DeliveryRoot
}

type EmittedCommand struct {
	Handle      string            `json:"handle"`
	Argv        []string          `json:"argv"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env,omitempty"`
	LaunchNonce string            `json:"launch_nonce,omitempty"`
	Line        string            `json:"line"`
}

type CreateResult struct {
	Outcome        Outcome `json:"outcome"`
	ActionRequired bool    `json:"action_required"`
	Profile        string  `json:"profile,omitempty"`
	// Binding is a managed backend's candidate runtime record. The
	// reconciliation engine is the only layer allowed to persist it under the
	// session lease. plan_only backends leave it empty.
	Binding         BindingRecord                `json:"binding,omitempty"`
	CaptureEvidence map[string][]CaptureEvidence `json:"-"`
	Commands        []EmittedCommand             `json:"commands,omitempty"`
	Plan            []byte                       `json:"plan,omitempty"`
	Reason          string                       `json:"reason,omitempty"`
}

type InspectRequest struct {
	Binding BindingRecord
	Root    *fsq.DeliveryRoot
}

type InspectResult struct {
	Status         InspectStatus `json:"status"`
	Evidence       string        `json:"evidence"`
	ActionRequired bool          `json:"action_required"`
}

type CloseRequest struct {
	Binding BindingRecord
	Root    *fsq.DeliveryRoot
}

type CloseResult struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}

type FocusRequest struct {
	Binding BindingRecord
	Root    *fsq.DeliveryRoot
}

type FocusResult struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}
