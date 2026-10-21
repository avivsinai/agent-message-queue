package cli

import (
	"testing"
)

func TestReportUnavailableCompanionsOnWindowsKeepsKeepaliveEnabled(t *testing.T) {
	all := true
	stdout, _, err := captureEnvOutput(t, func() error {
		return reportUnavailableCompanionsOnWindows(&all, "windows")
	})
	if err != nil {
		t.Fatalf("reportUnavailableCompanionsOnWindows: %v", err)
	}
	if !all {
		t.Fatal("--all was disabled even though amq-keepalive is available on Windows")
	}
	if stdout != "--all: only amq-keepalive is published for Windows; skipping amq-bridge, amq-acp, amq-remote, and amq-owner-sign\n" {
		t.Fatalf("stdout = %q, want exact Windows skip line", stdout)
	}
}
