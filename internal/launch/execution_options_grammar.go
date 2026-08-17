package launch

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

type PrepareExecutionOptionsPresence struct {
	Injector bool
	Symphony bool
}

var allowedSymphonyEvents = []string{"after_create", "before_run", "after_run", "before_remove"}

// ValidatePrepareExecutionOptionsGrammar is the single input-grammar validator
// for public launch intents and the managed execution-options codec.
func ValidatePrepareExecutionOptionsGrammar(options PrepareExecutionOptions, presence PrepareExecutionOptionsPresence) error {
	injectorConfigured := presence.Injector || options.InjectorMode != "" || options.InjectorVia != "" || len(options.InjectorArgs) != 0
	symphonyConfigured := presence.Symphony || len(options.SymphonyEvents) != 0 || options.SymphonyWorkspaceKey != ""

	switch options.WakeMode {
	case "disabled":
		if strings.TrimSpace(options.AuditReason) == "" {
			return fmt.Errorf("disabled wake requires an audit reason")
		}
		if options.RequireWake {
			return fmt.Errorf("require_wake conflicts with disabled wake")
		}
		if injectorConfigured {
			return fmt.Errorf("disabled wake forbids injector settings")
		}
	case "enabled":
		if options.AuditReason != "" {
			return fmt.Errorf("enabled wake forbids a disabled-wake audit reason")
		}
	default:
		return fmt.Errorf("invalid wake mode %q", options.WakeMode)
	}

	if injectorConfigured {
		if !slices.Contains([]string{"auto", "raw", "paste", "none"}, options.InjectorMode) {
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
	}

	if symphonyConfigured {
		if len(options.SymphonyEvents) == 0 {
			return fmt.Errorf("symphony requires at least one event")
		}
		seen := make(map[string]struct{}, len(options.SymphonyEvents))
		for _, event := range options.SymphonyEvents {
			if !slices.Contains(allowedSymphonyEvents, event) {
				return fmt.Errorf("unknown symphony event %q", event)
			}
			if _, ok := seen[event]; ok {
				return fmt.Errorf("duplicate symphony event %q", event)
			}
			seen[event] = struct{}{}
		}
		if !utf8.ValidString(options.SymphonyWorkspaceKey) || strings.ContainsRune(options.SymphonyWorkspaceKey, 0) {
			return fmt.Errorf("symphony workspace key is invalid")
		}
	}
	return nil
}
