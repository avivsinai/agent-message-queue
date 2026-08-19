package launchapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidateDigest(digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("digest must be canonical sha256:<lowercase-hex>")
	}
	return nil
}

func DecodePrepareRequestV1(data []byte) (PrepareRequestV1, error) {
	var request PrepareRequestV1
	if err := decodeStrictJSON(data, &request); err != nil {
		return PrepareRequestV1{}, fmt.Errorf("decode prepare request v1: %w", err)
	}
	if err := validateCallerContextJSON(data); err != nil {
		return PrepareRequestV1{}, fmt.Errorf("decode prepare request v1: %w", err)
	}
	if err := request.Validate(); err != nil {
		return PrepareRequestV1{}, err
	}
	return request, nil
}

func (request PrepareRequestV1) Validate() error {
	if request.RequestVersion != RequestVersionV1 {
		return fmt.Errorf("unsupported prepare request version %d", request.RequestVersion)
	}
	if err := request.Target.validate(); err != nil {
		return err
	}
	if !slices.Contains([]string{"auto", "cmux", "ghostty", "tmux", "commands"}, request.Launcher) {
		return fmt.Errorf("unsupported launcher %q", request.Launcher)
	}
	if request.Placement != nil {
		if err := request.Placement.Validate(); err != nil {
			return err
		}
	}
	if err := internallaunch.ValidateCallerContext(request.CallerContext); err != nil {
		return err
	}
	return request.Intent.Validate()
}

func DecodeApplyRequestV1(data []byte) (ApplyRequestV1, error) {
	if !utf8.Valid(data) {
		return ApplyRequestV1{}, fmt.Errorf("decode apply request v1: invalid UTF-8")
	}
	var request ApplyRequestV1
	if err := decodeStrictJSON(data, &request); err != nil {
		return ApplyRequestV1{}, fmt.Errorf("decode apply request v1: %w", err)
	}
	prepare, present, err := rawObjectField(data, "prepare")
	if err != nil {
		return ApplyRequestV1{}, fmt.Errorf("decode apply request v1: %w", err)
	}
	if present {
		if err := validateCallerContextJSON(prepare); err != nil {
			return ApplyRequestV1{}, fmt.Errorf("decode apply request v1: prepare: %w", err)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return ApplyRequestV1{}, fmt.Errorf("inspect apply request fields: %w", err)
	}
	if fields["decisions"] == nil || bytes.Equal(bytes.TrimSpace(fields["decisions"]), []byte("null")) {
		return ApplyRequestV1{}, fmt.Errorf("apply request requires decisions array")
	}
	if err := request.Validate(); err != nil {
		return ApplyRequestV1{}, err
	}
	return request, nil
}

func validateCallerContextJSON(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	raw, present, err := rawObjectField(data, "caller_context")
	if err != nil || !present {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("caller_context must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("caller_context: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("caller_context must be an object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("caller_context: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("caller_context key must be a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("caller_context contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("caller_context[%q]: %w", key, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("caller_context: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("caller_context has trailing JSON")
	}
	return nil
}

func rawObjectField(data []byte, field string) (json.RawMessage, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, false, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, false, fmt.Errorf("request must be an object")
	}
	var found json.RawMessage
	present := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, fmt.Errorf("request field name must be a string")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, false, err
		}
		if key == field {
			if present {
				return nil, false, fmt.Errorf("duplicate field %q", field)
			}
			found, present = raw, true
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false, fmt.Errorf("request has trailing JSON")
	}
	return found, present, nil
}

func (request ApplyRequestV1) Validate() error {
	if request.RequestVersion != RequestVersionV1 {
		return fmt.Errorf("unsupported apply request version %d", request.RequestVersion)
	}
	if err := request.Prepare.Validate(); err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	if request.SubjectSchema != 0 && request.SubjectSchema != SubjectSchemaV1 && request.SubjectSchema != SubjectSchemaV2 {
		return fmt.Errorf("unsupported subject schema %d", request.SubjectSchema)
	}
	if err := ValidateDigest(request.SubjectDigest); err != nil {
		return fmt.Errorf("subject_digest: %w", err)
	}
	seen := make(map[string]struct{}, len(request.Decisions))
	allowedChoices := []DecisionChoiceV1{
		DecisionTrustExactSubject,
		DecisionDeny,
		DecisionFreshOnce,
		DecisionAbort,
		DecisionCloseOld,
		DecisionLeaveOld,
		DecisionAcceptDegraded,
	}
	for i, decision := range request.Decisions {
		if strings.TrimSpace(decision.ActionID) == "" || strings.TrimSpace(string(decision.Choice)) == "" {
			return fmt.Errorf("decisions[%d] requires action_id and choice", i)
		}
		if _, ok := seen[decision.ActionID]; ok {
			return fmt.Errorf("duplicate decision for action %q", decision.ActionID)
		}
		if !slices.Contains(allowedChoices, decision.Choice) {
			return fmt.Errorf("decisions[%d] has invalid choice %q", i, decision.Choice)
		}
		seen[decision.ActionID] = struct{}{}
	}
	return nil
}

func DecodeInspectRequestV1(data []byte) (InspectRequestV1, error) {
	var request InspectRequestV1
	if err := decodeStrictJSON(data, &request); err != nil {
		return InspectRequestV1{}, fmt.Errorf("decode inspect request v1: %w", err)
	}
	if err := request.validate(); err != nil {
		return InspectRequestV1{}, err
	}
	return request, nil
}

func DecodeFocusRequestV1(data []byte) (FocusRequestV1, error) {
	request, err := DecodeInspectRequestV1(data)
	return FocusRequestV1(request), err
}

func DecodeCloseRequestV1(data []byte) (CloseRequestV1, error) {
	request, err := DecodeInspectRequestV1(data)
	return CloseRequestV1(request), err
}

func (request InspectRequestV1) validate() error {
	if request.RequestVersion != RequestVersionV1 {
		return fmt.Errorf("unsupported lifecycle request version %d", request.RequestVersion)
	}
	return request.Target.validate()
}

func (target TargetV1) validate() error {
	if !cleanAbsolutePath(target.ProjectRoot) {
		return fmt.Errorf("target project_root must be a clean absolute path")
	}
	if target.BaseRoot != "" && !cleanAbsolutePath(target.BaseRoot) {
		return fmt.Errorf("target base_root must be a clean absolute path")
	}
	if !cleanAbsolutePath(target.SessionRoot) {
		return fmt.Errorf("target session_root must be a clean absolute path")
	}
	if err := fsq.ValidateHandle(target.Session); err != nil {
		return fmt.Errorf("invalid target session: %w", err)
	}
	return nil
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}
