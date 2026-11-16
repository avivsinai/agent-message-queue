package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
)

// After an upgrade the endpoint keeps the binary it started with; doctor
// notes the version it reports when that is not this binary's.
func TestDoctorNotesAnEndpointOfAnotherVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := os.MkdirTemp("", "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, stateDirName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := ipc.Listen(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.SetVersion("0.0.1")
	go func() { _ = srv.Serve(ctx) }()

	notes := func() []string {
		var details []string
		noteEndpointVersion(func(_, _, detail, _ string) { details = append(details, detail) }, root, stateDir)
		return details
	}
	got := notes()
	if len(got) != 1 || !strings.Contains(got[0], "runs amq-remote 0.0.1") {
		t.Fatalf("notes = %q, want one that says the endpoint runs amq-remote 0.0.1", got)
	}
	srv.SetVersion(version)
	if got := notes(); len(got) != 0 {
		t.Fatalf("endpoint at this version = %q, want no note", got)
	}
}
