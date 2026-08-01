package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionGuardRouteExplainSessionPinABIIsJSONExitZero(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	foreignProject := filepath.Join(parent, "foreign")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice", "bob")
	foreignRoot := sessionRoot(t, foreignProject, "session1", "alice", "bob")
	pinSendSessionForTest(t, globalBase, globalRoot, "session1")

	// runRouteExplainJSONForTest fails the test if runRouteExplain returns an
	// error. A mismatched source therefore proves the route probe's promised
	// JSON-error/exit-0 channel rather than an exit-5 process failure.
	result := runRouteExplainJSONForTest(t,
		"--from-root", foreignRoot,
		"--me", "alice",
		"--to", "bob",
	)
	if result.Routable {
		t.Fatalf("mismatched pinned source was reported routable: %#v", result)
	}
	if result.Error == "" || !strings.Contains(result.Error, "mismatched source") {
		t.Fatalf("route JSON error = %q, want pinned-source mismatch", result.Error)
	}
}

func TestSessionGuardRouteExplainInvalidPinABIIsJSONExitZero(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	foreignProject := filepath.Join(parent, "foreign")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice", "bob")
	foreignRoot := sessionRoot(t, foreignProject, "session1", "alice", "bob")
	t.Setenv(envRoot, globalRoot)
	t.Setenv(envBaseRoot, globalBase)
	t.Setenv(envSession, "session1")
	t.Setenv(envRootID, "malformed-root-token")
	t.Setenv(envBaseRootID, "malformed-base-token")

	// The route helper converts guard errors into its JSON result and returns
	// nil, so malformed pin evidence must remain a probe error with exit 0.
	result := runRouteExplainJSONForTest(t,
		"--from-root", foreignRoot,
		"--me", "alice",
		"--to", "bob",
	)
	if result.Routable {
		t.Fatalf("invalid pinned source was reported routable: %#v", result)
	}
	if result.Error == "" || !strings.Contains(result.Error, "identity pin") {
		t.Fatalf("route JSON error = %q, want invalid identity-pin diagnostic", result.Error)
	}
}
