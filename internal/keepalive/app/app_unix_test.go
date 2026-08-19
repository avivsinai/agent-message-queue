//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

func TestAttachRefusesHostileRegistryWithoutStartingWake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string) (registryPath, root, target string)
	}{
		{
			name: "symlink dir",
			setup: func(t *testing.T, dir string) (string, string, string) {
				t.Helper()
				realDir := filepath.Join(dir, "real-registry")
				if err := os.Mkdir(realDir, 0o700); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
				linkDir := filepath.Join(dir, "link-registry")
				if err := os.Symlink(realDir, linkDir); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
				root := filepath.Join(dir, "amq-root")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatalf("Mkdir root: %v", err)
				}
				return filepath.Join(linkDir, "registry.json"), root, filepath.Join(dir, "inbox.txt")
			},
		},
		{
			name: "duplicate ids",
			setup: func(t *testing.T, dir string) (string, string, string) {
				t.Helper()
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatalf("Chmod registry dir: %v", err)
				}
				root := filepath.Join(dir, "amq-root")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatalf("Mkdir root: %v", err)
				}
				path := filepath.Join(dir, "registry.json")
				entry := registry.Entry{
					ID:   registry.EntryID(root, "codex", "file", filepath.Join(dir, "inbox.txt")),
					Root: root, Agent: "codex", Adapter: "file", Target: filepath.Join(dir, "inbox.txt"),
					State: registry.StateAttached,
				}
				data, err := json.Marshal(registry.File{SchemaVersion: registry.SchemaVersion, Entries: []registry.Entry{entry, entry}})
				if err != nil {
					t.Fatalf("Marshal: %v", err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return path, root, filepath.Join(dir, "other-inbox.txt")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registryPath, root, target := tc.setup(t, dir)
			amqCalls := filepath.Join(dir, "amq-calls.log")
			fakeAMQ := filepath.Join(dir, "amq")
			if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nprintf wake >> \"$AMQ_KEEPALIVE_AMQ_CALLS\"\nexit 7\n"), 0o700); err != nil {
				t.Fatalf("write fake amq: %v", err)
			}
			t.Setenv("AMQ_KEEPALIVE_AMQ_CALLS", amqCalls)
			var stderr bytes.Buffer
			code := (App{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{
				"attach", "--registry", registryPath, "--adapter", "file", "--target", target,
				"--root", root, "--base-root", dir, "--session", "amq-root", "--me", "codex", "--amq", fakeAMQ,
			})
			if code == 0 {
				t.Fatalf("attach code=0 stderr=%s, want refusal", stderr.String())
			}
			if _, err := os.Stat(amqCalls); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused attach started a wake: stat err=%v", err)
			}
		})
	}
}

func TestCanonicalExistingPathFailsClosedOnELOOPAndEACCES(t *testing.T) {
	dir := t.TempDir()
	loopA := filepath.Join(dir, "loop-a")
	loopB := filepath.Join(dir, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatalf("Symlink A: %v", err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatalf("Symlink B: %v", err)
	}
	got, err := canonicalExistingPath(loopA)
	if err == nil || got != "" {
		t.Fatalf("canonicalExistingPath(ELOOP) = %q, %v; want error not identity", got, err)
	}
	if !errors.Is(err, syscall.ELOOP) && !strings.Contains(strings.ToLower(err.Error()), "too many links") {
		t.Fatalf("canonicalExistingPath(ELOOP) error = %v, want ELOOP", err)
	}

	denied := filepath.Join(dir, "denied")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatalf("Mkdir denied: %v", err)
	}
	child := filepath.Join(denied, "root")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("Mkdir child: %v", err)
	}
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatalf("Chmod denied: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })
	got, err = canonicalExistingPath(child)
	_ = os.Chmod(denied, 0o700)
	if err == nil || got != "" {
		t.Fatalf("canonicalExistingPath(EACCES) = %q, %v; want error not identity", got, err)
	}
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, os.ErrPermission) {
		t.Fatalf("canonicalExistingPath(EACCES) error = %v, want EACCES", err)
	}
}
