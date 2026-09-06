//go:build linux

package cli

import (
	"strings"
	"testing"
)

func TestLinuxProcStartTokenHandlesParensInCommand(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[19] = "123456"
	stat := "42 (amq wake ) with spaces) " + strings.Join(fields, " ")

	token, err := linuxProcStartToken(stat)
	if err != nil {
		t.Fatalf("linuxProcStartToken: %v", err)
	}
	if token != "123456" {
		t.Fatalf("token = %q, want 123456", token)
	}
}
