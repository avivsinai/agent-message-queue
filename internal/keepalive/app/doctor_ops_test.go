package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

// TestDoctorOpsMarksLocklessCompanionStale (611.13.2 d): doctor --ops must
// present a companion row whose lifetime lock is not held as STALE, never
// active; a live row (lock held) as held; and count them in the report.
func TestDoctorOpsMarksLocklessCompanionStale(t *testing.T) {
	root, err := os.MkdirTemp("", "amqdoctor-ops")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	regPath := filepath.Join(root, "registry.json")
	store := registry.New(regPath)

	// Two companions on separate roots (the registry refuses two agents on
	// one target): stale (no lock) and live (lock held by this test).
	companions := []struct {
		dir   string
		agent string
		slot  *string
	}{
		{filepath.Join(root, "stale"), "agent-stale", new(string)},
		{filepath.Join(root, "live"), "agent-live", new(string)},
	}
	for _, c := range companions {
		if err := os.MkdirAll(c.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		up, err := store.Upsert(registry.Entry{Root: c.dir, Agent: c.agent, Adapter: "remote", Target: c.dir, State: registry.StateActive})
		if err != nil {
			t.Fatal(err)
		}
		*c.slot = up.ID
	}
	// Hold the live companion's lifetime lock.
	lockFile, err := os.OpenFile(registry.LifetimeLockPath(regPath, *companions[1].slot), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockFile.Close() }()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	var stdout, stderr bytes.Buffer
	code := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"doctor", "--registry", regPath, "--ops"})
	if code != 0 {
		t.Fatalf("doctor --ops exited %d, stderr: %s", code, stderr.String())
	}
	var report struct {
		Companions []struct {
			ID    string `json:"id"`
			Lock  string `json:"lock"`
			State string `json:"state"`
		} `json:"companions"`
		Stale int `json:"stale"`
		Live  int `json:"live"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("parse report %q: %v", stdout.String(), err)
	}
	if report.Stale != 1 || report.Live != 1 {
		t.Fatalf("stale=%d live=%d, want 1/1; report=%s", report.Stale, report.Live, stdout.String())
	}
	for _, c := range report.Companions {
		switch c.ID {
		case *companions[0].slot:
			if c.Lock != "not-held" || c.State != "stale" {
				t.Fatalf("phantom row lock=%q state=%q, want not-held/stale", c.Lock, c.State)
			}
		case *companions[1].slot:
			if c.Lock != "held" || c.State == "stale" {
				t.Fatalf("live row lock=%q state=%q, want held/active", c.Lock, c.State)
			}
		}
	}
}

// TestDoctorOpsWithoutFlagKeepsFullRegistry pins that plain doctor output is
// unchanged (full registry JSON) so --ops is purely additive.
func TestDoctorOpsWithoutFlagKeepsFullRegistry(t *testing.T) {
	root, err := os.MkdirTemp("", "amqdoctor-plain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	regPath := filepath.Join(root, "registry.json")
	store := registry.New(regPath)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(registry.Entry{Root: root, Agent: "agent-x", Adapter: "remote", Target: root, State: registry.StateActive}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"doctor", "--registry", regPath})
	if code != 0 {
		t.Fatalf("doctor exited %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"entries"`) {
		t.Fatalf("plain doctor must print the full registry file, got: %s", stdout.String())
	}
}
