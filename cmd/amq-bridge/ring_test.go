package main

import (
	"strings"
	"testing"
)

func TestRunRingUnitRejectsMissingFlags(t *testing.T) {
	if err := runRing([]string{}); err == nil || !strings.Contains(err.Error(), "ring adapter is required") {
		t.Fatalf("missing adapter error = %v", err)
	}
	if err := runRing([]string{"grokbot"}); err == nil || !strings.Contains(err.Error(), "--key-file is required") {
		t.Fatalf("missing key-file error = %v", err)
	}
	if err := runRing([]string{"grokbot", "--key-file", "/tmp/ring.json"}); err == nil || !strings.Contains(err.Error(), "handle and pending are required") {
		t.Fatalf("missing argv error = %v", err)
	}
	if err := runRing([]string{"nope", "--key-file", "/tmp/ring.json", "grok", "1"}); err == nil || !strings.Contains(err.Error(), "unknown ring adapter") {
		t.Fatalf("unknown adapter error = %v", err)
	}
}
