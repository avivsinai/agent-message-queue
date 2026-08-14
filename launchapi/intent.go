package launchapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

func DecodeLaunchIntentV1(data []byte) (LaunchIntentV1, error) {
	var intent LaunchIntentV1
	if err := decodeStrictJSON(data, &intent); err != nil {
		return LaunchIntentV1{}, fmt.Errorf("decode launch intent v1: %w", err)
	}
	return intent, nil
}

func (intent *LaunchIntentV1) UnmarshalJSON(data []byte) error {
	type wireLaunchIntentV1 LaunchIntentV1
	var decoded wireLaunchIntentV1
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	if err := requireLaunchIntentFields(data); err != nil {
		return err
	}
	value := LaunchIntentV1(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*intent = value
	return nil
}

func MarshalLaunchIntentV1(intent LaunchIntentV1) ([]byte, error) {
	if err := intent.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (intent LaunchIntentV1) Validate() error {
	if intent.IntentVersion != IntentVersionV1 {
		return fmt.Errorf("unsupported launch intent version %d", intent.IntentVersion)
	}
	if len(intent.Participants) == 0 {
		return fmt.Errorf("launch intent requires at least one participant")
	}
	seen := make(map[string]struct{}, len(intent.Participants))
	for i, participant := range intent.Participants {
		if err := participant.validate(); err != nil {
			return fmt.Errorf("participants[%d]: %w", i, err)
		}
		if _, ok := seen[participant.Handle]; ok {
			return fmt.Errorf("duplicate participant handle %q", participant.Handle)
		}
		seen[participant.Handle] = struct{}{}
	}
	return nil
}

func (participant ParticipantV1) validate() error {
	if err := fsq.ValidateHandle(participant.Handle); err != nil {
		return fmt.Errorf("invalid handle: %w", err)
	}
	if !participant.Runnable {
		if participant.Executable != "" || participant.Args != nil || participant.Cwd != nil ||
			participant.EnvOverlay != nil || participant.ResumePolicy != "" || participant.Execution != nil {
			return fmt.Errorf("non-runnable participant %q must be handle-only", participant.Handle)
		}
		return nil
	}
	if participant.Executable == "" {
		return fmt.Errorf("runnable participant %q requires executable", participant.Handle)
	}
	if participant.Cwd == nil {
		return fmt.Errorf("runnable participant %q requires cwd", participant.Handle)
	}
	if err := participant.Cwd.validate(); err != nil {
		return fmt.Errorf("runnable participant %q cwd: %w", participant.Handle, err)
	}
	switch participant.ResumePolicy {
	case ResumePolicyResume, ResumePolicyFresh, ResumePolicyDisabled:
	default:
		return fmt.Errorf("runnable participant %q has invalid resume policy %q", participant.Handle, participant.ResumePolicy)
	}
	if participant.Execution == nil {
		return fmt.Errorf("runnable participant %q requires execution options", participant.Handle)
	}
	if err := participant.Execution.validate(); err != nil {
		return fmt.Errorf("runnable participant %q execution: %w", participant.Handle, err)
	}
	if _, err := internallaunch.ValidateStaticProviderInput(
		participant.Executable,
		participant.Args,
		participant.EnvOverlay,
	); err != nil {
		return fmt.Errorf("runnable participant %q: %w", participant.Handle, err)
	}
	return nil
}

func (cwd WorkingDirectoryV1) validate() error {
	if cwd.Path == "" || !utf8.ValidString(cwd.Path) || strings.ContainsRune(cwd.Path, 0) {
		return fmt.Errorf("path is invalid")
	}
	if filepath.Clean(cwd.Path) != cwd.Path {
		return fmt.Errorf("path must be clean")
	}
	switch cwd.Kind {
	case WorkingDirectoryRelative:
		if filepath.IsAbs(cwd.Path) {
			return fmt.Errorf("relative path must not be absolute")
		}
		for _, part := range strings.FieldsFunc(cwd.Path, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == ".." {
				return fmt.Errorf("relative path must not escape its project root")
			}
		}
	case WorkingDirectoryAbsolute:
		if !filepath.IsAbs(cwd.Path) {
			return fmt.Errorf("absolute path is required")
		}
	default:
		return fmt.Errorf("invalid kind %q", cwd.Kind)
	}
	return nil
}

func (options ExecutionOptionsV1) validate() error {
	wake := options.Wake
	switch wake.Mode {
	case WakeDisabled:
		if strings.TrimSpace(wake.AuditReason) == "" {
			return fmt.Errorf("disabled wake requires an audit reason")
		}
		if options.RequireWake {
			return fmt.Errorf("require_wake conflicts with disabled wake")
		}
		if wake.Injector != nil {
			return fmt.Errorf("disabled wake forbids injector settings")
		}
	case WakeEnabled:
		if wake.AuditReason != "" {
			return fmt.Errorf("enabled wake forbids a disabled-wake audit reason")
		}
	default:
		return fmt.Errorf("invalid wake mode %q", wake.Mode)
	}
	if wake.Injector != nil {
		if err := wake.Injector.validate(); err != nil {
			return err
		}
	}
	if options.Integrations.Symphony != nil {
		if err := options.Integrations.Symphony.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (injector InjectorOptionsV1) validate() error {
	switch injector.Mode {
	case InjectorAuto, InjectorRaw, InjectorPaste, InjectorNone:
	default:
		return fmt.Errorf("invalid injector mode %q", injector.Mode)
	}
	if injector.Mode == InjectorNone && (injector.Via != "" || len(injector.Args) > 0) {
		return fmt.Errorf("injector mode none forbids via and args")
	}
	if injector.Via != "" {
		if !filepath.IsAbs(injector.Via) || filepath.Clean(injector.Via) != injector.Via || strings.ContainsRune(injector.Via, 0) {
			return fmt.Errorf("injector via must be a clean absolute path")
		}
	} else if len(injector.Args) > 0 {
		return fmt.Errorf("injector args require via")
	}
	for _, arg := range injector.Args {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("injector args contain an invalid value")
		}
	}
	return nil
}

func (symphony SymphonyOptionsV1) validate() error {
	if len(symphony.Events) == 0 {
		return fmt.Errorf("symphony requires at least one event")
	}
	seen := make(map[SymphonyEvent]struct{}, len(symphony.Events))
	allowed := []SymphonyEvent{
		SymphonyAfterCreate,
		SymphonyBeforeRun,
		SymphonyAfterRun,
		SymphonyBeforeRemove,
	}
	for _, event := range symphony.Events {
		if !slices.Contains(allowed, event) {
			return fmt.Errorf("unknown symphony event %q", event)
		}
		if _, ok := seen[event]; ok {
			return fmt.Errorf("duplicate symphony event %q", event)
		}
		seen[event] = struct{}{}
	}
	if strings.ContainsRune(symphony.WorkspaceKey, 0) || !utf8.ValidString(symphony.WorkspaceKey) {
		return fmt.Errorf("symphony workspace_key is invalid")
	}
	return nil
}

func requireLaunchIntentFields(data []byte) error {
	var document struct {
		IntentVersion json.RawMessage              `json:"intent_version"`
		Participants  []map[string]json.RawMessage `json:"participants"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("inspect launch intent fields: %w", err)
	}
	if document.IntentVersion == nil {
		return fmt.Errorf("launch intent requires intent_version")
	}
	if document.Participants == nil {
		return fmt.Errorf("launch intent requires participants")
	}
	for i, fields := range document.Participants {
		if fields["handle"] == nil {
			return fmt.Errorf("participants[%d] requires handle", i)
		}
		if fields["runnable"] == nil {
			return fmt.Errorf("participants[%d] requires runnable", i)
		}
		if bytes.Equal(bytes.TrimSpace(fields["runnable"]), []byte("null")) {
			return fmt.Errorf("participants[%d] runnable must be a boolean", i)
		}
		var runnable bool
		if err := json.Unmarshal(fields["runnable"], &runnable); err != nil {
			return fmt.Errorf("participants[%d] runnable: %w", i, err)
		}
		if !runnable && len(fields) != 2 {
			return fmt.Errorf("participants[%d] non-runnable participant must contain exactly handle and runnable", i)
		}
		if runnable {
			for _, required := range []string{"executable", "cwd", "resume_policy", "execution"} {
				if fields[required] == nil {
					return fmt.Errorf("participants[%d] requires %s", i, required)
				}
			}
			if err := requireObjectFields(fields["cwd"], fmt.Sprintf("participants[%d].cwd", i), "kind", "path"); err != nil {
				return err
			}
			for _, optional := range []string{"args", "env_overlay"} {
				if err := rejectExplicitNull(fields, optional, fmt.Sprintf("participants[%d]", i)); err != nil {
					return err
				}
			}
			var execution map[string]json.RawMessage
			if err := json.Unmarshal(fields["execution"], &execution); err != nil || execution == nil {
				return fmt.Errorf("participants[%d].execution must be an object", i)
			}
			for _, required := range []string{"require_wake", "no_gitignore", "wake"} {
				if execution[required] == nil {
					return fmt.Errorf("participants[%d].execution requires %s", i, required)
				}
			}
			if err := requireObjectFields(execution["wake"], fmt.Sprintf("participants[%d].execution.wake", i), "mode"); err != nil {
				return err
			}
			if err := rejectExplicitNull(execution, "integrations", fmt.Sprintf("participants[%d].execution", i)); err != nil {
				return err
			}
			var wake map[string]json.RawMessage
			if err := json.Unmarshal(execution["wake"], &wake); err != nil {
				return fmt.Errorf("participants[%d].execution.wake must be an object", i)
			}
			if err := rejectExplicitNull(wake, "injector", fmt.Sprintf("participants[%d].execution.wake", i)); err != nil {
				return err
			}
			if integrationsRaw := execution["integrations"]; integrationsRaw != nil {
				var integrations map[string]json.RawMessage
				if err := json.Unmarshal(integrationsRaw, &integrations); err != nil || integrations == nil {
					return fmt.Errorf("participants[%d].execution.integrations must be an object", i)
				}
				if err := rejectExplicitNull(integrations, "symphony", fmt.Sprintf("participants[%d].execution.integrations", i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func rejectExplicitNull(fields map[string]json.RawMessage, field, context string) error {
	raw, present := fields[field]
	if present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s.%s must not be null", context, field)
	}
	return nil
}

func requireObjectFields(raw json.RawMessage, context string, required ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("%s must be an object", context)
	}
	for _, field := range required {
		if fields[field] == nil || bytes.Equal(bytes.TrimSpace(fields[field]), []byte("null")) {
			return fmt.Errorf("%s requires %s", context, field)
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
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
