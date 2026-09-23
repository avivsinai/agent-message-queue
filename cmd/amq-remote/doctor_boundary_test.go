package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// 611.18: doctor names each failing boundary with a remedy and exits 6.
func TestDoctorNamesFailingBoundaries(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(relayStatusDoc{URL: "wss://relay.example", Shares: []relayShareStatus{{
		Session: "work", Target: "cx", State: "authenticated", Commands: "closed: DM channel membership is not owner and body only",
	}}})
	if err := os.WriteFile(filepath.Join(stateDir, relayStatusFile), status, 0o600); err != nil {
		t.Fatal(err)
	}
	out, code, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	if code != protocol.ExitActionRequired {
		t.Fatalf("exit = %d, want 6", code)
	}
	got := map[string]bool{}
	for _, f := range out.(map[string]any)["failing"].([]boundaryFailure) {
		if f.Remedy == "" {
			t.Fatalf("boundary %s has no remedy", f.Boundary)
		}
		got[f.Boundary] = true
	}
	if !got["endpoint"] || !got["dm_surface"] || got["relay_auth"] {
		t.Fatalf("failing boundaries = %v, want endpoint and dm_surface only", got)
	}
}

// codex #869 r1: a minted body with no owner-signed generation reported
// attestation=missing but no failing boundary.
func TestDoctorListsAnUnenrolledBody(t *testing.T) {
	root := t.TempDir()
	if _, err := bodykey.Mint(filepath.Join(root, "extensions", "remote", "keys", "work")); err != nil {
		t.Fatal(err)
	}
	out, _, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range out.(map[string]any)["failing"].([]boundaryFailure) {
		if f.Boundary == "body_key" && f.Subject == "work" && f.Remedy != "" {
			return
		}
	}
	t.Fatalf("unenrolled body not listed: %v", out.(map[string]any)["failing"])
}
