package main

import "testing"

func TestGetVersionPrefersInjectedVersion(t *testing.T) {
	previous := version
	version = "v1.2.3"
	t.Cleanup(func() { version = previous })

	if got := getVersion(); got != "v1.2.3" {
		t.Fatalf("getVersion() = %q, want v1.2.3", got)
	}
}
