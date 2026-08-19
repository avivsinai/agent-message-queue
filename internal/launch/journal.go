package launch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	JournalVersion  = 1
	journalFilename = "journal.json"
)

var stableTreeIdentity = fsq.StableTreeIdentity
var stableTreeIdentityInfo = fsq.StableTreeIdentityInfo

type JournalPhase string

const (
	JournalIntent  JournalPhase = "intent"
	JournalCreated JournalPhase = "created"
)

// LaunchJournal is a recovery transaction, not a launcher binding. A resource
// recorded here is never owned or adoptable until a backend proves its live
// identity or a matching authoritative binding already exists.
type LaunchJournal struct {
	Version          int                    `json:"version"`
	Phase            JournalPhase           `json:"phase"`
	ProjectIdentity  string                 `json:"project_identity"`
	RootIdentity     string                 `json:"root_identity"`
	ProjectPhysical  string                 `json:"project_physical_identity,omitempty"`
	RootPhysical     string                 `json:"root_physical_identity,omitempty"`
	Session          string                 `json:"session"`
	Backend          string                 `json:"backend"`
	Profile          string                 `json:"profile"`
	HostIdentity     string                 `json:"host_identity"`
	InstanceIdentity string                 `json:"instance_identity"`
	RosterDigest     string                 `json:"roster_digest"`
	PlanDigest       string                 `json:"plan_digest"`
	LaunchNonce      string                 `json:"launch_nonce"`
	CreatedAt        time.Time              `json:"created_at"`
	Plan             Plan                   `json:"plan"`
	Agents           []AgentReconcileResult `json:"agents"`
	Conversations    []ConversationRecord   `json:"conversations"`
	Binding          *BindingRecord         `json:"binding,omitempty"`
	JoinBinding      *BindingRecord         `json:"join_binding,omitempty"`
	JoinDeltas       []JoinDelta            `json:"join_deltas,omitempty"`
	Placement        PlacementPreview       `json:"placement,omitempty"`
	CallerContext    map[string]string      `json:"caller_context,omitempty"`
	// CandidateBinding is an immutable backend marker for an exact resource
	// discovered before reconciliation can publish the created phase.
	CandidateBinding *BindingRecord `json:"candidate_binding,omitempty"`
}

// JoinDelta is the durable proof for one window added to an existing managed
// tmux session. Its session identity is checked before the next mutation.
type JoinDelta struct {
	Handle       string `json:"handle"`
	SessionID    string `json:"session_id"`
	SessionName  string `json:"session_name"`
	SessionNonce string `json:"session_nonce"`
	Epoch        string `json:"epoch,omitempty"`
	WindowID     string `json:"window_id"`
	PaneID       string `json:"pane_id"`
}

func (record LaunchJournal) Validate() error {
	if record.Version != JournalVersion {
		return fmt.Errorf("unsupported launch journal version %d", record.Version)
	}
	if record.Phase != JournalIntent && record.Phase != JournalCreated {
		return fmt.Errorf("invalid launch journal phase %q", record.Phase)
	}
	for name, value := range map[string]string{
		"project identity": record.ProjectIdentity, "root identity": record.RootIdentity,
		"session": record.Session, "backend": record.Backend, "profile": record.Profile,
		"host identity": record.HostIdentity, "instance identity": record.InstanceIdentity,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !filepath.IsAbs(record.ProjectIdentity) || filepath.Clean(record.ProjectIdentity) != record.ProjectIdentity {
		return fmt.Errorf("project identity must be a canonical absolute path")
	}
	if !filepath.IsAbs(record.RootIdentity) || filepath.Clean(record.RootIdentity) != record.RootIdentity {
		return fmt.Errorf("root identity must be a canonical absolute path")
	}
	if strings.TrimSpace(record.ProjectPhysical) == "" || strings.TrimSpace(record.RootPhysical) == "" {
		return fmt.Errorf("launch journal physical identities are required")
	}
	if !canonicalSessionPattern.MatchString(record.Session) || strings.HasPrefix(record.Session, "-") {
		return fmt.Errorf("invalid launch journal session %q", record.Session)
	}
	if err := validateOptionalPlacement(record.Placement); err != nil {
		return fmt.Errorf("launch journal placement: %w", err)
	}
	if err := ValidateCallerContext(record.CallerContext); err != nil {
		return err
	}
	if record.JoinBinding == nil && len(record.JoinDeltas) != 0 {
		return fmt.Errorf("launch journal join deltas require a join binding")
	}
	if record.JoinBinding != nil {
		if err := record.JoinBinding.Validate(); err != nil {
			return fmt.Errorf("launch journal join binding: %w", err)
		}
		if record.JoinBinding.Backend != record.Backend || record.JoinBinding.Profile != record.Profile ||
			record.JoinBinding.HostIdentity != record.HostIdentity || record.JoinBinding.InstanceIdentity != record.InstanceIdentity {
			return fmt.Errorf("launch journal join binding context does not match")
		}
		sessionID, sessionName, err := parseTmuxSessionResource(*record.JoinBinding)
		expectedSessionName, nameErr := tmuxSessionName(record.ProjectIdentity, record.Session)
		if err != nil || nameErr != nil || sessionName != expectedSessionName {
			return fmt.Errorf("launch journal join binding session does not match")
		}
		epoch := parseTmuxEpoch(*record.JoinBinding)
		seenJoinHandles := make(map[string]struct{}, len(record.JoinDeltas))
		for i, delta := range record.JoinDeltas {
			if err := fsq.ValidateHandle(delta.Handle); err != nil {
				return fmt.Errorf("launch journal join delta %d: %w", i, err)
			}
			if _, ok := seenJoinHandles[delta.Handle]; ok {
				return fmt.Errorf("duplicate launch journal join delta handle %q", delta.Handle)
			}
			seenJoinHandles[delta.Handle] = struct{}{}
			if delta.SessionID != sessionID || delta.SessionName != sessionName || delta.SessionNonce != record.JoinBinding.LaunchNonce ||
				delta.Epoch != epoch || delta.WindowID == "" || !tmuxPaneID(delta.PaneID) {
				return fmt.Errorf("launch journal join delta %q identity does not match its binding", delta.Handle)
			}
		}
	}
	if !validUUID(record.LaunchNonce) {
		return fmt.Errorf("launch journal nonce must be a UUID")
	}
	if record.CreatedAt.IsZero() || record.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("launch journal creation time must be UTC")
	}
	decodedRoster, err := hex.DecodeString(record.RosterDigest)
	if err != nil || len(decodedRoster) != sha256.Size {
		return fmt.Errorf("launch journal roster digest must be SHA-256")
	}
	decodedPlan, err := hex.DecodeString(strings.TrimPrefix(record.PlanDigest, "sha256:"))
	if !strings.HasPrefix(record.PlanDigest, "sha256:") || err != nil || len(decodedPlan) != sha256.Size {
		return fmt.Errorf("launch journal plan digest must be SHA-256")
	}
	if err := record.Plan.Validate(); err != nil {
		return fmt.Errorf("launch journal plan: %w", err)
	}
	digest, err := record.Plan.SemanticDigest()
	if err != nil {
		return err
	}
	if digest != record.PlanDigest {
		return fmt.Errorf("launch journal plan digest mismatch")
	}
	if len(record.Agents) == 0 || len(record.Agents) != len(record.Plan.Agents) || len(record.Conversations) != len(record.Plan.Agents) {
		return fmt.Errorf("launch journal roster lengths do not match")
	}
	agentHandles := make(map[string]struct{}, len(record.Agents))
	for i, agent := range record.Agents {
		if err := fsq.ValidateHandle(agent.Handle); err != nil {
			return fmt.Errorf("launch journal result %d: %w", i, err)
		}
		if _, ok := agentHandles[agent.Handle]; ok {
			return fmt.Errorf("duplicate launch journal result handle %q", agent.Handle)
		}
		agentHandles[agent.Handle] = struct{}{}
	}
	seen := make(map[string]struct{}, len(record.Plan.Agents))
	for i, plan := range record.Plan.Agents {
		if plan.LaunchNonce != record.LaunchNonce || record.Conversations[i].Handle != plan.Handle {
			return fmt.Errorf("launch journal agent %d does not match its plan", i)
		}
		if _, ok := agentHandles[plan.Handle]; !ok {
			return fmt.Errorf("launch journal plan handle %q has no result", plan.Handle)
		}
		if err := record.Conversations[i].Validate(); err != nil {
			return fmt.Errorf("launch journal conversation %q: %w", plan.Handle, err)
		}
		conversation := record.Conversations[i]
		if conversation.State == CapturePending && conversation.LaunchNonce != record.LaunchNonce {
			return fmt.Errorf("launch journal pending conversation %q has a different nonce", plan.Handle)
		}
		if conversation.State == CaptureReady && plan.ConversationID != "" && plan.ConversationID != conversation.Identity.ID {
			return fmt.Errorf("launch journal resume identity for %q does not match its conversation", plan.Handle)
		}
		if _, ok := seen[plan.Handle]; ok {
			return fmt.Errorf("duplicate launch journal handle %q", plan.Handle)
		}
		seen[plan.Handle] = struct{}{}
	}
	if record.Phase == JournalIntent {
		if record.Binding != nil {
			return fmt.Errorf("intent journal must not contain a binding")
		}
		if record.CandidateBinding != nil {
			if err := record.CandidateBinding.Validate(); err != nil {
				return fmt.Errorf("intent journal candidate binding: %w", err)
			}
			if !journalMatchesBinding(record, *record.CandidateBinding) {
				return fmt.Errorf("intent journal candidate binding does not match its launch generation")
			}
		}
		return nil
	}
	if record.CandidateBinding != nil {
		return fmt.Errorf("created journal must not retain candidate binding")
	}
	if record.Binding == nil {
		return fmt.Errorf("created journal requires a candidate binding")
	}
	if err := record.Binding.Validate(); err != nil {
		return fmt.Errorf("created journal binding: %w", err)
	}
	if !journalMatchesBinding(record, *record.Binding) {
		return fmt.Errorf("created journal binding does not match its launch generation")
	}
	return nil
}

func NewLaunchJournal(request ReconcileRequest, backend string, detect DetectResult, plan Plan, planDigest, nonce string, agents []AgentReconcileResult, conversations []ConversationRecord, now time.Time) (LaunchJournal, error) {
	projectIdentity, err := canonicalIdentity(request.ProjectRoot)
	if err != nil {
		return LaunchJournal{}, fmt.Errorf("resolve project identity: %w", err)
	}
	rootIdentity, err := canonicalIdentity(request.Root.Base())
	if err != nil {
		return LaunchJournal{}, fmt.Errorf("resolve session root identity: %w", err)
	}
	rosterDigest, err := projectConfigDigest(request.Config)
	if err != nil {
		return LaunchJournal{}, err
	}
	preview, err := ResolvePlacement(backend, request.Placement)
	if err != nil {
		return LaunchJournal{}, err
	}
	record := LaunchJournal{
		Version: JournalVersion, Phase: JournalIntent, ProjectIdentity: projectIdentity,
		RootIdentity: rootIdentity, Session: request.Session, Backend: backend,
		Profile: detect.Profile.Identity(), HostIdentity: detect.HostIdentity,
		InstanceIdentity: detect.InstanceIdentity, RosterDigest: rosterDigest,
		PlanDigest: planDigest, LaunchNonce: nonce, CreatedAt: now.UTC(), Plan: plan,
		Agents: slices.Clone(agents), Conversations: slices.Clone(conversations),
		Placement:     preview,
		CallerContext: cloneCallerContext(request.CallerContext),
	}
	record.ProjectPhysical, err = stableTreeIdentity(projectIdentity)
	if err != nil {
		return LaunchJournal{}, fmt.Errorf("resolve project physical identity: %w", err)
	}
	record.RootPhysical, err = stableTreeIdentityInfo(request.Root.FileInfo())
	if err != nil {
		return LaunchJournal{}, fmt.Errorf("resolve session root physical identity: %w", err)
	}
	if err := record.Validate(); err != nil {
		return LaunchJournal{}, err
	}
	return record, nil
}

func (record LaunchJournal) ValidateRequest(request ReconcileRequest) error {
	projectIdentity, err := canonicalIdentity(request.ProjectRoot)
	if err != nil {
		return err
	}
	rootIdentity, err := canonicalIdentity(request.Root.Base())
	if err != nil {
		return err
	}
	rosterDigest, err := projectConfigDigest(request.Config)
	if err != nil {
		return err
	}
	if record.ProjectIdentity != projectIdentity || record.RootIdentity != rootIdentity || record.Session != request.Session {
		return fmt.Errorf("launch journal belongs to a different project or session root")
	}
	projectPhysical, err := stableTreeIdentity(projectIdentity)
	if err != nil {
		return fmt.Errorf("resolve project physical identity: %w", err)
	}
	rootPhysical, err := stableTreeIdentityInfo(request.Root.FileInfo())
	if err != nil {
		return fmt.Errorf("resolve session root physical identity: %w", err)
	}
	if (record.ProjectPhysical != "" && record.ProjectPhysical != projectPhysical) ||
		(record.RootPhysical != "" && record.RootPhysical != rootPhysical) {
		return fmt.Errorf("launch journal physical project or session root changed")
	}
	if record.RosterDigest != rosterDigest {
		return fmt.Errorf("launch journal roster changed since resource creation")
	}
	if !reflect.DeepEqual(record.CallerContext, request.CallerContext) {
		return fmt.Errorf("launch journal caller context changed")
	}
	if err := journalPlacementMatches(record, request); err != nil {
		return err
	}
	seenExecution := make(map[string]struct{}, len(record.Plan.Agents))
	for _, agent := range record.Plan.Agents {
		seenExecution[agent.Handle] = struct{}{}
		options, ok := request.ExecutionOptions[agent.Handle]
		if !ok {
			if agent.Execution != nil {
				return fmt.Errorf("launch journal execution options changed for %q", agent.Handle)
			}
			continue
		}
		if !reflect.DeepEqual(agent.Execution, clonePrepareExecutionOptions(&options)) {
			return fmt.Errorf("launch journal execution options changed for %q", agent.Handle)
		}
	}
	for handle := range request.ExecutionOptions {
		if _, ok := seenExecution[handle]; !ok {
			return fmt.Errorf("launch journal has no execution options carrier for %q", handle)
		}
	}
	return nil
}

func WriteJournal(root *fsq.DeliveryRoot, lease *Lease, record LaunchJournal) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	_, err = root.WriteFileAtomic(bindingDirectory, journalFilename, append(data, '\n'), 0o600)
	return err
}

func LoadJournal(root *fsq.DeliveryRoot) (LaunchJournal, error) {
	if root == nil {
		return LaunchJournal{}, fmt.Errorf("missing pinned session root")
	}
	file, info, err := root.OpenRegularNoFollow(filepath.Join(bindingDirectory, journalFilename))
	if err != nil {
		return LaunchJournal{}, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return LaunchJournal{}, fmt.Errorf("launch journal permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return LaunchJournal{}, err
	}
	var record LaunchJournal
	if err := decodeStrict(data, &record); err != nil {
		return LaunchJournal{}, fmt.Errorf("decode launch journal: %w", err)
	}
	if err := record.Validate(); err != nil {
		return LaunchJournal{}, err
	}
	return record, nil
}

func ClearJournal(root *fsq.DeliveryRoot, lease *Lease, expected LaunchJournal) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	current, err := LoadJournal(root)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return fmt.Errorf("launch journal changed before clear")
	}
	return root.Remove(filepath.Join(bindingDirectory, journalFilename))
}

// ClearJournalRaw removes a journal after an exact byte-for-byte compare. It
// is reserved for operator cleanup of a journal whose semantic validation
// fails, while retaining the same lease and replacement guard as ClearJournal.
func ClearJournalRaw(root *fsq.DeliveryRoot, lease *Lease, expected []byte) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	current, err := root.ReadFile(filepath.Join(bindingDirectory, journalFilename))
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("launch journal changed before clear")
	}
	return root.Remove(filepath.Join(bindingDirectory, journalFilename))
}

func JournalPath(sessionRoot string) string {
	return filepath.Join(sessionRoot, bindingDirectory, journalFilename)
}

func validateOptionalPlacement(preview PlacementPreview) error {
	if preview.Requested == nil && preview.Effective.Target == "" && preview.Effective.Layout == "" {
		return nil
	}
	if err := preview.Effective.Validate(); err != nil {
		return err
	}
	if preview.Requested != nil {
		return preview.Requested.Validate()
	}
	return nil
}

func journalPlacementMatches(record LaunchJournal, request ReconcileRequest) error {
	legacy := record.Placement.Requested == nil && record.Placement.Effective.Target == ""
	if legacy {
		if request.Placement != nil {
			return fmt.Errorf("launch journal placement changed since resource creation")
		}
		return nil
	}
	got, err := ResolvePlacement(record.Backend, request.Placement)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, record.Placement) {
		return fmt.Errorf("launch journal placement changed since resource creation")
	}
	return nil
}

func journalMatchesBinding(journal LaunchJournal, binding BindingRecord) bool {
	launchNonce := journal.LaunchNonce
	if journal.JoinBinding != nil {
		launchNonce = journal.JoinBinding.LaunchNonce
	}
	return binding.Backend == journal.Backend && binding.Profile == journal.Profile &&
		binding.HostIdentity == journal.HostIdentity && binding.InstanceIdentity == journal.InstanceIdentity &&
		binding.LaunchNonce == launchNonce && reflect.DeepEqual(binding.CallerContext, journal.CallerContext)
}

func canonicalIdentity(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func projectConfigDigest(config ProjectConfig) (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func loadOptionalJournal(root *fsq.DeliveryRoot) (LaunchJournal, bool, error) {
	record, err := LoadJournal(root)
	if errors.Is(err, os.ErrNotExist) {
		return LaunchJournal{}, false, nil
	}
	return record, err == nil, err
}
