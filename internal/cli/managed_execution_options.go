package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const managedExecutionOptionsFlag = "execution-options"

var managedSymphonyEvents = []string{"after_create", "before_run", "after_run", "before_remove"}

func validateManagedExecutionOptions(options launch.PrepareExecutionOptions) error {
	switch options.WakeMode {
	case "disabled":
		if strings.TrimSpace(options.AuditReason) == "" {
			return fmt.Errorf("disabled wake requires an audit reason")
		}
		if options.RequireWake {
			return fmt.Errorf("require_wake conflicts with disabled wake")
		}
		if options.InjectorMode != "" || options.InjectorVia != "" || len(options.InjectorArgs) != 0 {
			return fmt.Errorf("disabled wake forbids injector settings")
		}
	case "enabled":
		if options.AuditReason != "" {
			return fmt.Errorf("enabled wake forbids a disabled-wake audit reason")
		}
	default:
		return fmt.Errorf("invalid wake mode %q", options.WakeMode)
	}

	switch options.InjectorMode {
	case "", "auto", "raw", "paste", "none":
	default:
		return fmt.Errorf("invalid injector mode %q", options.InjectorMode)
	}
	if options.InjectorMode == "none" && (options.InjectorVia != "" || len(options.InjectorArgs) != 0) {
		return fmt.Errorf("injector mode none forbids via and args")
	}
	if options.InjectorVia != "" {
		if !filepath.IsAbs(options.InjectorVia) || filepath.Clean(options.InjectorVia) != options.InjectorVia || strings.ContainsRune(options.InjectorVia, 0) {
			return fmt.Errorf("injector via must be a clean absolute path")
		}
	} else if len(options.InjectorArgs) != 0 {
		return fmt.Errorf("injector args require via")
	}
	for _, arg := range options.InjectorArgs {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("injector args contain an invalid value")
		}
	}

	seen := make(map[string]struct{}, len(options.SymphonyEvents))
	for _, event := range options.SymphonyEvents {
		if !slices.Contains(managedSymphonyEvents, event) {
			return fmt.Errorf("unknown symphony event %q", event)
		}
		if _, ok := seen[event]; ok {
			return fmt.Errorf("duplicate symphony event %q", event)
		}
		seen[event] = struct{}{}
	}
	if options.SymphonyWorkspaceKey != "" && len(options.SymphonyEvents) == 0 {
		return fmt.Errorf("symphony workspace key requires at least one event")
	}
	if !utf8.ValidString(options.SymphonyWorkspaceKey) || strings.ContainsRune(options.SymphonyWorkspaceKey, 0) {
		return fmt.Errorf("symphony workspace key is invalid")
	}
	return nil
}

func encodeManagedExecutionOptions(options launch.PrepareExecutionOptions) (string, error) {
	if err := validateManagedExecutionOptions(options); err != nil {
		return "", err
	}
	data, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeManagedExecutionOptions(encoded string) (launch.PrepareExecutionOptions, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return launch.PrepareExecutionOptions{}, fmt.Errorf("decode execution options: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var options launch.PrepareExecutionOptions
	if err := decoder.Decode(&options); err != nil {
		return launch.PrepareExecutionOptions{}, fmt.Errorf("decode execution options: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return launch.PrepareExecutionOptions{}, fmt.Errorf("decode execution options: trailing data")
	}
	if err := validateManagedExecutionOptions(options); err != nil {
		return launch.PrepareExecutionOptions{}, err
	}
	return options, nil
}
