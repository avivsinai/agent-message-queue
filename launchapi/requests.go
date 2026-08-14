package launchapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	return request.Intent.Validate()
}

func DecodeApplyRequestV1(data []byte) (ApplyRequestV1, error) {
	var request ApplyRequestV1
	if err := decodeStrictJSON(data, &request); err != nil {
		return ApplyRequestV1{}, fmt.Errorf("decode apply request v1: %w", err)
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

func (request ApplyRequestV1) Validate() error {
	if request.RequestVersion != RequestVersionV1 {
		return fmt.Errorf("unsupported apply request version %d", request.RequestVersion)
	}
	if err := request.Prepare.Validate(); err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	if err := ValidateDigest(request.SubjectDigest); err != nil {
		return fmt.Errorf("subject_digest: %w", err)
	}
	seen := make(map[string]struct{}, len(request.Decisions))
	allowedChoices := []string{
		"trust_exact_subject",
		"deny",
		"fresh_once",
		"abort",
		"close_old",
		"leave_old",
		"accept_degraded",
	}
	for i, decision := range request.Decisions {
		if strings.TrimSpace(decision.ActionID) == "" || strings.TrimSpace(decision.Choice) == "" {
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
