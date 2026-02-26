package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// Spec phase constants.
const (
	specPhaseResearch = "research"
	specPhaseExchange = "exchange"
	specPhaseDraft    = "draft"
	specPhaseReview   = "review"
	specPhaseConverge = "converge"
	specPhaseDone     = "done"
)

// specState represents the state of a collaborative spec workflow.
type specState struct {
	Topic       string                        `json:"topic"`
	Phase       string                        `json:"phase"`
	Started     string                        `json:"started"`
	StartedBy   string                        `json:"started_by"`
	Agents      []string                      `json:"agents"`
	Thread      string                        `json:"thread"`
	Submissions map[string]map[string]specSub `json:"submissions"`
	FinalSpec   string                        `json:"final_spec,omitempty"`
	Completed   string                        `json:"completed,omitempty"`
}

// specSub records a single agent's submission for a given phase.
type specSub struct {
	Submitted string `json:"submitted"`
	MsgID     string `json:"msg_id"`
	File      string `json:"file"`
}

func specStatePath(root, topic string) string {
	return fsq.SpecTopicDir(root, topic) + "/state.json"
}

func specLockPath(root, topic string) string {
	return fsq.SpecsDir(root) + "/." + topic + ".lock"
}

func loadSpecState(root, topic string) (specState, error) {
	path := specStatePath(root, topic)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return specState{}, fmt.Errorf("spec %q not found (no state.json)", topic)
		}
		return specState{}, fmt.Errorf("read spec state: %w", err)
	}
	var state specState
	if err := json.Unmarshal(data, &state); err != nil {
		return specState{}, fmt.Errorf("parse spec state: %w", err)
	}
	return state, nil
}

func saveSpecState(root, topic string, state specState) error {
	path := specStatePath(root, topic)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spec state: %w", err)
	}
	data = append(data, '\n')

	tmpPath := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync temp state file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic rename state file: %w", err)
	}
	return nil
}

func withSpecLock(root, topic string, fn func() error) error {
	lockPath := specLockPath(root, topic)
	if err := os.MkdirAll(fsq.SpecsDir(root), 0o700); err != nil {
		return fmt.Errorf("ensure specs dir: %w", err)
	}
	return lock.WithExclusiveFileLock(lockPath, fn)
}

// validSubmitPhase checks whether a submission for submitPhase is allowed in currentPhase.
func validSubmitPhase(currentPhase, submitPhase string) error {
	switch currentPhase {
	case specPhaseResearch:
		if submitPhase != specPhaseResearch {
			return fmt.Errorf("phase is %q: can only submit %q", currentPhase, specPhaseResearch)
		}
	case specPhaseExchange:
		if submitPhase != specPhaseDraft {
			return fmt.Errorf("phase is %q: can only submit %q", currentPhase, specPhaseDraft)
		}
	case specPhaseDraft:
		if submitPhase != specPhaseDraft {
			return fmt.Errorf("phase is %q: can only submit %q", currentPhase, specPhaseDraft)
		}
	case specPhaseReview:
		if submitPhase != specPhaseReview {
			return fmt.Errorf("phase is %q: can only submit %q", currentPhase, specPhaseReview)
		}
	case specPhaseConverge:
		if submitPhase != "final" {
			return fmt.Errorf("phase is %q: can only submit %q", currentPhase, "final")
		}
	case specPhaseDone:
		return fmt.Errorf("spec is already done")
	default:
		return fmt.Errorf("unknown phase: %q", currentPhase)
	}
	return nil
}

// advancePhase checks transition rules and advances the phase if conditions are met.
// Returns true if the phase was advanced.
func advancePhase(state *specState) bool {
	switch state.Phase {
	case specPhaseResearch:
		// All agents must have submitted research.
		if allAgentsSubmitted(state, specPhaseResearch) {
			state.Phase = specPhaseExchange
			return true
		}
	case specPhaseExchange:
		// First draft submitted moves to draft phase.
		// This is handled inline during submit (exchange→draft on first draft submit).
		// If somehow called here, check if any draft exists.
		if anyAgentSubmitted(state, specPhaseDraft) {
			state.Phase = specPhaseDraft
			return true
		}
	case specPhaseDraft:
		if allAgentsSubmitted(state, specPhaseDraft) {
			state.Phase = specPhaseReview
			return true
		}
	case specPhaseReview:
		if allAgentsSubmitted(state, specPhaseReview) {
			state.Phase = specPhaseConverge
			return true
		}
	case specPhaseConverge:
		if state.FinalSpec != "" {
			state.Phase = specPhaseDone
			state.Completed = time.Now().UTC().Format(time.RFC3339Nano)
			return true
		}
	}
	return false
}

func allAgentsSubmitted(state *specState, phase string) bool {
	for _, agent := range state.Agents {
		subs, ok := state.Submissions[agent]
		if !ok {
			return false
		}
		if _, ok := subs[phase]; !ok {
			return false
		}
	}
	return true
}

func anyAgentSubmitted(state *specState, phase string) bool {
	for _, agent := range state.Agents {
		if subs, ok := state.Submissions[agent]; ok {
			if _, ok := subs[phase]; ok {
				return true
			}
		}
	}
	return false
}
