package cli

import (
	"reflect"
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
	if stdout != "--all: only amq-keepalive is published for Windows; skipping amq-bridge and amq-acp\n" {
		t.Fatalf("stdout = %q, want exact Windows skip line", stdout)
	}
}

func TestCompanionBinariesAvailableOnFiltersWindowsOnly(t *testing.T) {
	names := []string{"amq-keepalive", "amq-bridge", "amq-acp"}
	if got := companionBinariesAvailableOn(names, "windows"); !reflect.DeepEqual(got, []string{"amq-keepalive"}) {
		t.Fatalf("Windows companions = %#v, want only amq-keepalive", got)
	}
	got := companionBinariesAvailableOn(names, "linux")
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("Linux companions = %#v, want %#v", got, names)
	}
	got[0] = "mutated"
	if names[0] != "amq-keepalive" {
		t.Fatal("companion filter aliased its input")
	}
}
