package format

import (
	"strings"
	"testing"
	"time"
)

func TestNewMessageID(t *testing.T) {
	id, err := NewMessageID(time.Date(2025, 12, 24, 15, 2, 33, 123000000, time.UTC))
	if err != nil {
		t.Fatalf("NewMessageID: %v", err)
	}
	if !strings.Contains(id, "_pid") {
		t.Fatalf("expected pid marker in id: %s", id)
	}
	if !strings.HasPrefix(id, "2025-12-24T15-02-33.123Z_") {
		t.Fatalf("unexpected prefix: %s", id)
	}
}
