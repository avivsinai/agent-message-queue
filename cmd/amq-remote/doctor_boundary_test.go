package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// Codex 2026-09-23T12-18-36.287Z_pid90763_92e75d76: doctor named no AMQ-route
// boundary when the endpoint handle was absent from config.json.
func TestDoctorNamesAMQRoute(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "extensions", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, code, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	if code != protocol.ExitActionRequired || !doctorHasBoundary(out, "amq_route", "remote") {
		t.Fatalf("missing route: exit=%d failing=%v", code, out.(map[string]any)["failing"])
	}
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte("{\"version\":1,\"created_utc\":\"2026-09-23T00:00:00Z\",\"agents\":[\"remote\"]}\n")
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	if doctorHasBoundary(out, "amq_route", "remote") {
		t.Fatalf("registered handle still failing: %v", out.(map[string]any)["failing"])
	}
	out, _, err = doctor([]string{"--root", root, "--me", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if !doctorHasBoundary(out, "amq_route", "other") {
		t.Fatalf("--me other not named: %v", out.(map[string]any)["failing"])
	}
}

func doctorHasBoundary(out any, boundary, subject string) bool {
	for _, f := range out.(map[string]any)["failing"].([]boundaryFailure) {
		if f.Boundary == boundary && f.Subject == subject && f.Remedy != "" {
			return true
		}
	}
	return false
}

// agent-message-queue-611.71: one unreadable request record made admission
// refuse every submit to its target ("reservation state undeterminable"),
// while doctor read the store through List, dropped the poison, and exited 0.
func TestDoctorNamesAnUnreadableRequestRecord(t *testing.T) {
	root := t.TempDir()
	hostDir := filepath.Join(root, "extensions", "remote", "v1", "requests", "host-1")
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	poison := filepath.Join(hostDir, "fake__deadbeef.json")
	if err := os.WriteFile(poison, []byte(`{"state":"runn`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code, err := doctor([]string{"--root", root})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range out.(map[string]any)["failing"].([]boundaryFailure) {
		if f.Boundary == "request_store" && f.Subject == "fake" && strings.Contains(f.Detail, poison) && f.Remedy != "" && code == protocol.ExitActionRequired {
			return
		}
	}
	t.Fatalf("unreadable record not named: exit=%d failing=%v", code, out.(map[string]any)["failing"])
}
