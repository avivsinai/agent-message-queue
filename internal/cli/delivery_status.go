package cli

import (
	"errors"
	"fmt"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func reportDeliveryError(messageID string, err error) error {
	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) {
		return err
	}
	return fmt.Errorf("message %s has a committed delivery; retrying may duplicate it: %w", messageID, err)
}

func outboxResult(err error) map[string]any {
	result := map[string]any{
		"written": err == nil,
		"error":   errString(err),
	}
	var committed *fsq.CommittedDurabilityError
	if errors.As(err, &committed) {
		result["written"] = true
		result["durability"] = "indeterminate"
		result["path"] = committed.FinalPath
	}
	return result
}

func reportOutboxError(err error) error {
	if err == nil {
		return nil
	}
	var committed *fsq.CommittedDurabilityError
	if errors.As(err, &committed) {
		return writeStderr("warning: outbox is written, but durability is indeterminate: %v\n", err)
	}
	return writeStderr("warning: outbox write failed: %v\n", err)
}
