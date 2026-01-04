package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

type headerValidator struct {
	strict bool
	known  map[string]struct{}
}

func newHeaderValidator(root string, strict bool) (*headerValidator, error) {
	if !strict {
		return &headerValidator{strict: false}, nil
	}
	known, err := loadKnownAgentSet(root, strict)
	if err != nil {
		return nil, err
	}
	return &headerValidator{strict: true, known: known}, nil
}

func (v *headerValidator) validate(header format.Header) error {
	if err := v.validateHeaderBasic(header); err != nil {
		return err
	}
	if len(v.known) == 0 {
		return nil
	}
	if _, ok := v.known[header.From]; !ok {
		return fmt.Errorf("unknown sender handle: %s", header.From)
	}
	for _, recipient := range header.To {
		if _, ok := v.known[recipient]; !ok {
			return fmt.Errorf("unknown recipient handle: %s", recipient)
		}
	}
	return nil
}

func (v *headerValidator) validateHeaderBasic(header format.Header) error {
	// Schema check only in strict mode - allows interop with older/newer clients
	if v.strict && header.Schema != format.CurrentSchema {
		return fmt.Errorf("unsupported schema: %d (expected %d)", header.Schema, format.CurrentSchema)
	}
	return validateHeaderFields(header)
}

// validateHeaderFields checks all header fields except schema version
func validateHeaderFields(header format.Header) error {
	if _, err := ensureSafeBaseName(header.ID); err != nil {
		return fmt.Errorf("invalid message id: %w", err)
	}
	if err := validateHandleValue("sender", header.From); err != nil {
		return err
	}
	if len(header.To) == 0 {
		return errors.New("missing recipients")
	}
	for _, recipient := range header.To {
		if err := validateHandleValue("recipient", recipient); err != nil {
			return err
		}
	}
	thread := strings.TrimSpace(header.Thread)
	if thread == "" {
		return errors.New("missing thread")
	}
	if thread != header.Thread {
		return errors.New("thread contains leading/trailing whitespace")
	}
	created := strings.TrimSpace(header.Created)
	if created == "" {
		return errors.New("missing created timestamp")
	}
	if created != header.Created {
		return errors.New("created timestamp contains leading/trailing whitespace")
	}
	if _, err := time.Parse(time.RFC3339Nano, header.Created); err != nil {
		return fmt.Errorf("invalid created timestamp: %w", err)
	}
	if !format.IsValidPriority(header.Priority) {
		return fmt.Errorf("invalid priority: %s", header.Priority)
	}
	if !format.IsValidKind(header.Kind) {
		return fmt.Errorf("invalid kind: %s", header.Kind)
	}
	return nil
}

func validateHandleValue(label, handle string) error {
	if strings.TrimSpace(handle) == "" {
		return fmt.Errorf("%s handle is empty", label)
	}
	norm, err := normalizeHandle(handle)
	if err != nil {
		return fmt.Errorf("invalid %s handle: %w", label, err)
	}
	if norm != handle {
		return fmt.Errorf("invalid %s handle (not normalized): %s", label, handle)
	}
	return nil
}

func safeHeaderID(id string) (string, bool) {
	if _, err := ensureSafeBaseName(id); err != nil {
		return "", false
	}
	return id, true
}
