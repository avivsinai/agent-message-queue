package launchapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
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
	if err := decodeStrictJSONValue(data, &decoded); err != nil {
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
			participant.Wrapper != nil || participant.InitialInput != nil || participant.EnvOverlay != nil || participant.ResumePolicy != "" ||
			participant.Execution != nil || participant.OnLive != "" {
			return fmt.Errorf("non-runnable participant %q must be handle-only", participant.Handle)
		}
		return nil
	}
	if participant.Executable == "" {
		return fmt.Errorf("runnable participant %q requires executable", participant.Handle)
	}
	if participant.Wrapper != nil {
		if err := participant.Wrapper.validate(); err != nil {
			return fmt.Errorf("runnable participant %q wrapper: %w", participant.Handle, err)
		}
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
	switch participant.OnLive {
	case "", OnLiveRefuse, OnLiveKeep:
	default:
		return fmt.Errorf("runnable participant %q has invalid on_live %q", participant.Handle, participant.OnLive)
	}
	if participant.Execution == nil {
		return fmt.Errorf("runnable participant %q requires execution options", participant.Handle)
	}
	if err := participant.Execution.validate(); err != nil {
		return fmt.Errorf("runnable participant %q execution: %w", participant.Handle, err)
	}
	if participant.InitialInput != nil {
		if err := participant.InitialInput.validate(); err != nil {
			return fmt.Errorf("runnable participant %q initial_input: %w", participant.Handle, err)
		}
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

func (wrapper WrapperV1) validate() error {
	if !utf8.ValidString(wrapper.Executable) || strings.ContainsRune(wrapper.Executable, 0) {
		return fmt.Errorf("executable must be valid UTF-8 without NUL")
	}
	if !filepath.IsAbs(wrapper.Executable) || filepath.Clean(wrapper.Executable) != wrapper.Executable {
		return fmt.Errorf("executable must be a clean absolute path")
	}
	for i, arg := range wrapper.Args {
		if arg == "" {
			return fmt.Errorf("args[%d] must not be empty", i)
		}
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("args[%d] must be valid UTF-8 without NUL", i)
		}
	}
	return nil
}

func (input InitialInputV1) validate() error {
	switch input.Kind {
	case InitialInputArgument, InitialInputStdin, InitialInputFile:
	default:
		return fmt.Errorf("invalid kind %q", input.Kind)
	}
	if !utf8.ValidString(input.Text) {
		return &InitialInputValidationError{Code: InitialInputInvalidUTF8, Detail: "text must be valid UTF-8"}
	}
	if strings.HasPrefix(input.Text, "-") {
		return &InitialInputValidationError{Code: InitialInputLeadingDash, Detail: "text must not begin with '-'"}
	}
	for _, r := range input.Text {
		if r <= 0x1f || r == 0x7f {
			return &InitialInputValidationError{Code: InitialInputControl, Detail: "text must not contain C0, DEL, CR, or LF control characters"}
		}
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
	return internallaunch.ValidatePrepareExecutionOptionsGrammar(toInternalExecutionOptions(options), internallaunch.PrepareExecutionOptionsPresence{
		Injector: options.Wake.Injector != nil,
		Symphony: options.Integrations.Symphony != nil,
	})
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
			for _, optional := range []string{"args", "env_overlay", "on_live"} {
				if err := rejectExplicitNull(fields, optional, fmt.Sprintf("participants[%d]", i)); err != nil {
					return err
				}
				if raw := fields["initial_input"]; raw != nil {
					if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
						return fmt.Errorf("participants[%d].initial_input must not be null", i)
					}
					if err := requireObjectFields(raw, fmt.Sprintf("participants[%d].initial_input", i), "kind", "text"); err != nil {
						return err
					}
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

// StrictJSONMaxDepth bounds the recursive structural scan performed before a
// public JSON document is decoded into a contract type.
const StrictJSONMaxDepth = 256

// StrictJSONErrorCode identifies a structural error found before decoding a
// public JSON document into its contract type.
type StrictJSONErrorCode string

const (
	StrictJSONDuplicateKey  StrictJSONErrorCode = "duplicate_json_key"
	StrictJSONDepthExceeded StrictJSONErrorCode = "depth_exceeded"
)

// StrictJSONError reports a structural JSON error with a stable machine code.
// Path and Key are retained as structured data so callers do not need to parse
// the human-readable error string.
type StrictJSONError struct {
	Code StrictJSONErrorCode
	Path string
	Key  string
}

func (e *StrictJSONError) Error() string {
	if e == nil {
		return "strict JSON decoding failed"
	}
	switch e.Code {
	case StrictJSONDuplicateKey:
		return fmt.Sprintf("duplicate key %q at %s (code=%s)", e.Key, e.Path, e.Code)
	case StrictJSONDepthExceeded:
		return fmt.Sprintf("JSON nesting exceeds maximum depth %d at %s (code=%s)", StrictJSONMaxDepth, e.Path, e.Code)
	default:
		return fmt.Sprintf("strict JSON decoding failed at %s (code=%s)", e.Path, e.Code)
	}
}

type strictJSONPathSegment struct {
	key   string
	index int
	array bool
}

func decodeStrictJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	if err := rejectDuplicateJSONKeys(scanner); err != nil {
		return err
	}
	if err := scanner.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return decodeStrictJSONValue(data, target)
}

func decodeStrictJSONValue(data []byte, target any) error {
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

// rejectDuplicateJSONKeys walks the complete JSON token stream. Decoder's
// struct handling silently keeps the last value for duplicate object keys, so
// this pass must happen before the contract decode and must recurse through
// arrays as well as objects. It carries path segments and renders them only
// when returning a typed error, keeping deeply nested failures linear.
func rejectDuplicateJSONKeys(decoder *json.Decoder) error {
	return rejectDuplicateJSONValue(decoder, nil, 0)
}

func rejectDuplicateJSONValue(decoder *json.Decoder, path []strictJSONPathSegment, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch delimiter := token.(type) {
	case json.Delim:
		if depth >= StrictJSONMaxDepth {
			return &StrictJSONError{Code: StrictJSONDepthExceeded, Path: renderStrictJSONPath(path)}
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("JSON object key at %s is not a string", renderStrictJSONPath(path))
				}
				if _, duplicate := seen[key]; duplicate {
					return &StrictJSONError{Code: StrictJSONDuplicateKey, Path: renderStrictJSONPath(path), Key: key}
				}
				seen[key] = struct{}{}
				childPath := append(path, strictJSONPathSegment{key: key})
				if err := rejectDuplicateJSONValue(decoder, childPath, depth+1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return fmt.Errorf("JSON object at %s is not closed", renderStrictJSONPath(path))
			}
		case '[':
			for index := 0; decoder.More(); index++ {
				childPath := append(path, strictJSONPathSegment{index: index, array: true})
				if err := rejectDuplicateJSONValue(decoder, childPath, depth+1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return fmt.Errorf("JSON array at %s is not closed", renderStrictJSONPath(path))
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, renderStrictJSONPath(path))
		}
	}
	return nil
}

func renderStrictJSONPath(path []strictJSONPathSegment) string {
	var builder strings.Builder
	builder.WriteByte('$')
	for _, segment := range path {
		if segment.array {
			fmt.Fprintf(&builder, "[%d]", segment.index)
			continue
		}
		builder.WriteByte('.')
		builder.WriteString(segment.key)
	}
	return builder.String()
}
