package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestFederationRequiresSourceProjectIdentityBeforeBody(t *testing.T) {
	clearSendMailboxTestEnv(t)
	fakeHome := t.TempDir()
	sourceRoot := filepath.Join(fakeHome, "mail")
	peerRoot := filepath.Join(t.TempDir(), "peer-mail")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, fakeHome, map[string]any{
		"root": sourceRoot,
		"peers": map[string]string{
			"peer": peerRoot,
		},
	})
	t.Setenv("HOME", fakeHome)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	missingBody := filepath.Join(t.TempDir(), "body-that-must-not-be-opened")
	_, _, sendErr := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--body", "@" + missingBody,
		})
	})
	if sendErr == nil {
		t.Fatal("cross-project send without source project identity succeeded")
	}
	if !strings.Contains(sendErr.Error(), "source project identity") {
		t.Fatalf("send error = %v, want source project identity refusal", sendErr)
	}
	if strings.Contains(sendErr.Error(), missingBody) {
		t.Fatalf("send read the body before federation validation: %v", sendErr)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("peer inbox count = %d, want 0", got)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer",
	)
	if result.Routable {
		t.Fatalf("route without source project identity was routable: %#v", result)
	}
	if !strings.Contains(result.Error, "source project identity") {
		t.Fatalf("route error = %q, want source project identity refusal", result.Error)
	}

	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "unidentified-federation",
		ReplyTo:      "bob",
		ReplyProject: "peer",
		FromProject:  "peer",
	})
	_, _, replyErr := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--body", "@" + missingBody,
		})
	})
	if replyErr == nil {
		t.Fatal("cross-project reply without source project identity succeeded")
	}
	if !strings.Contains(replyErr.Error(), "source project identity") {
		t.Fatalf("reply error = %v, want source project identity refusal", replyErr)
	}
	if strings.Contains(replyErr.Error(), missingBody) {
		t.Fatalf("reply read the body before federation validation: %v", replyErr)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("peer inbox count after reply = %d, want 0", got)
	}
}

func TestFederationExplicitProjectRoundTripAndRouteArgvParity(t *testing.T) {
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source-project")
	peerProjectDir := filepath.Join(t.TempDir(), "peer-project")
	sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
	peerBase := filepath.Join(peerProjectDir, ".agent-mail")
	sourceRoot := filepath.Join(sourceBase, "collab")
	peerRoot := filepath.Join(peerBase, "collab")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	writeRouteAmqrc(t, peerProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "peer",
		"peers": map[string]string{
			"source": sourceBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer",
	)
	if !result.Routable {
		t.Fatalf("explicit project route is not routable: %s", result.Error)
	}
	sendArgs := append([]string{}, result.Argv[2:]...)
	sendArgs = append(sendArgs, "--body", "federated request")
	_, _, err := captureEnvOutput(t, func() error { return runSend(sendArgs) })
	if err != nil {
		t.Fatalf("route argv was rejected by send: %v", err)
	}

	request := soleDeliveredMessage(t, peerRoot, "bob")
	if request.Header.ReplyProject != "source" || request.Header.FromProject != "source" {
		t.Fatalf("request project identity = reply:%q from:%q, want source", request.Header.ReplyProject, request.Header.FromProject)
	}
	if !strings.Contains(request.Header.Thread, "source:") {
		t.Fatalf("thread %q does not contain source project identity", request.Header.Thread)
	}

	_, _, err = captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", peerRoot,
			"--me", "bob",
			"--id", request.Header.ID,
			"--body", "federated response",
		})
	})
	if err != nil {
		t.Fatalf("cross-project reply: %v", err)
	}
	response := soleDeliveredMessage(t, sourceRoot, "alice")
	if response.Header.Thread != request.Header.Thread {
		t.Fatalf("reply thread = %q, want %q", response.Header.Thread, request.Header.Thread)
	}
	if response.Header.ReplyProject != "peer" || response.Header.FromProject != "peer" {
		t.Fatalf("response project identity = reply:%q from:%q, want peer", response.Header.ReplyProject, response.Header.FromProject)
	}
}

func TestFederationStrictSendUsesUnpinnedSourceBaseConfig(t *testing.T) {
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source-project")
	sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
	sourceRoot := filepath.Join(sourceBase, "collab")
	peerBase := filepath.Join(t.TempDir(), "peer-project", ".agent-mail")
	peerRoot := filepath.Join(peerBase, "collab")
	configureSendTestRoot(t, sourceBase, "carol")
	configureSendTestRoot(t, peerBase, "bob")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--strict",
			"--body", "must not bypass source base roster",
		})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "alice" not in config.json`) {
		t.Fatalf("strict cross-project send error = %v, want source base-roster refusal", err)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("strict source-roster refusal delivered %d message(s)", got)
	}
	if _, err := os.Lstat(filepath.Join(sourceRoot, "meta", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("strict send copied source base config into session: %v", err)
	}

	configureSendTestRoot(t, sourceBase, "alice")
	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--strict",
			"--body", "configured source sender",
		})
	}); err != nil {
		t.Fatalf("strict configured cross-project send: %v", err)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 1 {
		t.Fatalf("configured strict source delivered %d message(s), want 1", got)
	}
}

func TestFederationStrictReplyUsesUnpinnedSourceBaseConfig(t *testing.T) {
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source-project")
	sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
	sourceRoot := filepath.Join(sourceBase, "collab")
	peerBase := filepath.Join(t.TempDir(), "peer-project", ".agent-mail")
	peerRoot := filepath.Join(peerBase, "collab")
	configureSendTestRoot(t, sourceBase, "carol")
	configureSendTestRoot(t, peerBase, "bob")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "strict-source-roster-reply",
		ReplyTo:      "bob@collab",
		ReplyProject: "peer",
		FromProject:  "peer",
	})
	missingBody := filepath.Join(t.TempDir(), "reply-body-must-not-be-read")
	_, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--strict",
			"--body", "@" + missingBody,
		})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "alice" not in config.json`) {
		t.Fatalf("strict cross-project reply error = %v, want source base-roster refusal", err)
	}
	if strings.Contains(err.Error(), missingBody) {
		t.Fatalf("strict reply read body before source base-roster refusal: %v", err)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("strict source-roster refusal delivered %d reply message(s)", got)
	}
}

func TestFederationStrictReplyUsesPeerBaseConfig(t *testing.T) {
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source-project")
	sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "collab")
	peerBase := filepath.Join(t.TempDir(), "peer-project", ".agent-mail")
	peerRoot := filepath.Join(peerBase, "collab")
	ensureRouteAgents(t, sourceRoot, "alice")
	configureSendTestRoot(t, peerBase, "carol")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "strict-peer-base-roster",
		ReplyTo:      "bob@collab",
		ReplyProject: "peer",
		FromProject:  "peer",
	})
	_, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--strict",
			"--body", "must not bypass peer base roster",
		})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "bob" not in config.json`) {
		t.Fatalf("strict cross-project reply error = %v, want peer base-roster refusal", err)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("strict peer-roster refusal delivered %d message(s)", got)
	}
	if _, err := os.Lstat(filepath.Join(peerRoot, "meta", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("strict reply copied peer base config into session: %v", err)
	}
}

func TestPlanDeliveryRouteUsesOneAmqrcSnapshot(t *testing.T) {
	sourceRoot := t.TempDir()
	peerA := filepath.Join(t.TempDir(), "peer-a")
	peerB := filepath.Join(t.TempDir(), "peer-b")
	results := []amqrcResult{
		{
			Config: amqrc{
				Project: "source-a",
				Root:    sourceRoot,
				Peers:   map[string]string{"peer": peerA},
			},
			Dir: t.TempDir(),
		},
		{
			Config: amqrc{
				Project: "source-b",
				Root:    sourceRoot,
				Peers:   map[string]string{"peer": peerB},
			},
			Dir: t.TempDir(),
		},
	}
	oldLookup := findDeliveryRouteAmqrc
	calls := 0
	findDeliveryRouteAmqrc = func(string) (amqrcResult, error) {
		result := results[0]
		if calls < len(results) {
			result = results[calls]
		}
		calls++
		return result, nil
	}
	t.Cleanup(func() { findDeliveryRouteAmqrc = oldLookup })

	plan, err := planDeliveryRoute(sourceRoot, "peer", "", deliveryRouteOptions{})
	if err != nil {
		t.Fatalf("plan delivery route: %v", err)
	}
	if calls != 1 {
		t.Fatalf("delivery route config reads = %d, want one immutable snapshot", calls)
	}
	if plan.SourceProject != "source-a" ||
		plan.PeerBaseRoot != peerA ||
		plan.DeliveryRoot != peerA {
		t.Fatalf("mixed delivery route generations: %#v", plan)
	}
}

func TestFederationRejectsSourceRootOutsideOwningAmqrc(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, sourceRoot, peerRoot string) string
	}{
		{
			name: "send",
			run: func(t *testing.T, sourceRoot, _ string) string {
				missingBody := filepath.Join(t.TempDir(), "body-must-not-be-read")
				_, _, err := captureEnvOutput(t, func() error {
					return runSend([]string{
						"--root", sourceRoot,
						"--me", "alice",
						"--to", "bob",
						"--project", "peer",
						"--session", "collab",
						"--body", "@" + missingBody,
					})
				})
				if err == nil {
					return ""
				}
				if strings.Contains(err.Error(), missingBody) {
					t.Fatalf("send read body before source ownership refusal: %v", err)
				}
				return err.Error()
			},
		},
		{
			name: "route explain",
			run: func(t *testing.T, sourceRoot, _ string) string {
				result := runRouteExplainJSONForTest(t,
					"--from-root", sourceRoot,
					"--me", "alice",
					"--to", "bob",
					"--project", "peer",
					"--session", "collab",
				)
				if result.Routable {
					return ""
				}
				return result.Error
			},
		},
		{
			name: "reply",
			run: func(t *testing.T, sourceRoot, _ string) string {
				originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
					From:         "bob",
					To:           []string{"alice"},
					Thread:       "unowned-federation-source",
					ReplyTo:      "bob@collab",
					ReplyProject: "peer",
					FromProject:  "peer",
				})
				missingBody := filepath.Join(t.TempDir(), "reply-body-must-not-be-read")
				_, _, err := captureEnvOutput(t, func() error {
					return runReply([]string{
						"--root", sourceRoot,
						"--me", "alice",
						"--id", originalID,
						"--body", "@" + missingBody,
					})
				})
				if err == nil {
					return ""
				}
				if strings.Contains(err.Error(), missingBody) {
					t.Fatalf("reply read body before source ownership refusal: %v", err)
				}
				return err.Error()
			},
		},
	}
	for _, sourceKind := range []string{"sibling", "symlinked session"} {
		if sourceKind == "symlinked session" && runtime.GOOS == "windows" {
			continue
		}
		for _, test := range tests {
			t.Run(sourceKind+"/"+test.name, func(t *testing.T) {
				clearSendMailboxTestEnv(t)
				sourceProjectDir := filepath.Join(t.TempDir(), "source-project")
				configuredBase := filepath.Join(sourceProjectDir, ".agent-mail")
				sourceRoot := filepath.Join(sourceProjectDir, "unowned-mail")
				if sourceKind == "symlinked session" {
					outsideRoot := filepath.Join(t.TempDir(), "outside-root")
					ensureRouteAgents(t, outsideRoot, "alice")
					if err := os.MkdirAll(configuredBase, 0o700); err != nil {
						t.Fatal(err)
					}
					sourceRoot = filepath.Join(configuredBase, "collab")
					if err := os.Symlink(outsideRoot, sourceRoot); err != nil {
						t.Fatal(err)
					}
				} else {
					ensureRouteAgents(t, sourceRoot, "alice")
				}
				peerBase := filepath.Join(t.TempDir(), "peer-project", ".agent-mail")
				peerRoot := filepath.Join(peerBase, "collab")
				ensureRouteAgents(t, peerRoot, "bob")
				configureSendTestRoot(t, peerBase, "bob")
				writeRouteAmqrc(t, sourceProjectDir, map[string]any{
					"root":    filepath.Base(configuredBase),
					"project": "source",
					"peers": map[string]string{
						"peer": peerBase,
					},
				})
				resetAmqrcCache()
				t.Cleanup(resetAmqrcCache)

				got := test.run(t, sourceRoot, peerRoot)
				if !strings.Contains(got, "does not own source root") {
					t.Fatalf("federation error = %q, want source-root ownership refusal", got)
				}
				if got := inboxCount(t, peerRoot, "bob"); got != 0 {
					t.Fatalf("ownership refusal delivered %d peer message(s)", got)
				}
			})
		}
	}
}

func TestFederationBaseRootRoundTripDoesNotMirrorReplySession(t *testing.T) {
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source-project")
	peerProjectDir := filepath.Join(t.TempDir(), "peer-project")
	sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
	sourceSessionDecoy := filepath.Join(sourceBase, "qa")
	peerBase := filepath.Join(peerProjectDir, ".agent-mail")
	peerRoot := filepath.Join(peerBase, "qa")
	ensureRouteAgents(t, sourceBase, "alice")
	ensureRouteAgents(t, sourceSessionDecoy, "alice")
	ensureRouteAgents(t, peerBase, "bob")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	writeRouteAmqrc(t, peerProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "peer",
		"peers": map[string]string{
			"source": sourceBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", sourceBase,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--session", "qa",
			"--body", "request from base",
		})
	})
	if err != nil {
		t.Fatalf("base-to-peer-session send: %v", err)
	}
	request := soleDeliveredMessage(t, peerRoot, "bob")
	if request.Header.ReplyTo != "alice" {
		t.Fatalf("reply_to = %q, want bare base-root handle", request.Header.ReplyTo)
	}

	_, _, err = captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", peerRoot,
			"--me", "bob",
			"--id", request.Header.ID,
			"--body", "response to base",
		})
	})
	if err != nil {
		t.Fatalf("peer-session reply to base: %v", err)
	}
	if got := inboxCount(t, sourceBase, "alice"); got != 1 {
		t.Fatalf("source base inbox count = %d, want 1", got)
	}
	if got := inboxCount(t, sourceSessionDecoy, "alice"); got != 0 {
		t.Fatalf("source session decoy inbox count = %d, want 0", got)
	}
	response := soleDeliveredMessage(t, sourceBase, "alice")
	if response.Header.ReplyTo != "bob@qa" {
		t.Fatalf("peer-session reply_to = %q, want bob@qa", response.Header.ReplyTo)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceBase,
			"--me", "alice",
			"--id", response.Header.ID,
			"--body", "second response to peer session",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("base reply back to peer session: %v", err)
	}
	var followupResult struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &followupResult); err != nil {
		t.Fatalf("decode second reply output: %v (output: %s)", err, stdout)
	}
	followup, err := format.ReadMessageFile(
		filepath.Join(fsq.AgentInboxNew(peerRoot, "bob"), followupResult.ID+".md"),
	)
	if err != nil {
		t.Fatalf("read second reply in peer session: %v", err)
	}
	if followup.Body != "second response to peer session\n" {
		t.Fatalf("second reply body = %q", followup.Body)
	}
	if followup.Header.ReplyTo != "alice" {
		t.Fatalf("base-source reply_to = %q, want alice", followup.Header.ReplyTo)
	}
	if got := inboxCount(t, peerBase, "bob"); got != 0 {
		t.Fatalf("peer base decoy inbox count = %d, want 0", got)
	}
}

func TestFederationReplyRejectsMalformedReplyToBeforeBodyOrDelivery(t *testing.T) {
	for _, replyTo := range []string{
		"",
		"@qa",
		"bob@",
		"bob@qa@extra",
		" bob@qa",
	} {
		t.Run(replyTo, func(t *testing.T) {
			clearSendMailboxTestEnv(t)
			sourceProjectDir := filepath.Join(t.TempDir(), "source")
			sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "collab")
			peerBase := filepath.Join(t.TempDir(), "peer-mail")
			peerRoot := filepath.Join(peerBase, "qa")
			ensureRouteAgents(t, sourceRoot, "alice")
			ensureRouteAgents(t, peerRoot, "bob")
			writeRouteAmqrc(t, sourceProjectDir, map[string]any{
				"root":    ".agent-mail",
				"project": "source",
				"peers": map[string]string{
					"peer": peerBase,
				},
			})
			resetAmqrcCache()
			t.Cleanup(resetAmqrcCache)

			originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
				From:         "bob",
				To:           []string{"alice"},
				Thread:       "malformed-federation-route",
				ReplyTo:      replyTo,
				ReplyProject: "peer",
				FromProject:  "peer",
			})
			missingBody := filepath.Join(t.TempDir(), "body-must-not-be-read")
			_, _, err := captureEnvOutput(t, func() error {
				return runReply([]string{
					"--root", sourceRoot,
					"--me", "alice",
					"--id", originalID,
					"--body", "@" + missingBody,
				})
			})
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("reply_to %q error = %v, want malformed route refusal", replyTo, err)
			}
			if strings.Contains(err.Error(), missingBody) {
				t.Fatalf("reply_to %q read body before route refusal: %v", replyTo, err)
			}
			if got := inboxCount(t, peerRoot, "bob"); got != 0 {
				t.Fatalf("reply_to %q delivered %d message(s), want 0", replyTo, got)
			}
		})
	}
}

func TestParseReplyToRouteShapes(t *testing.T) {
	for _, test := range []struct {
		name          string
		raw           string
		allowBase     bool
		wantHandle    string
		wantSession   string
		wantMalformed bool
	}{
		{name: "peer base", raw: "bob", allowBase: true, wantHandle: "bob"},
		{name: "session", raw: "bob@qa", allowBase: true, wantHandle: "bob", wantSession: "qa"},
		{name: "same project requires session", raw: "bob", wantMalformed: true},
		{name: "leading separator", raw: "@qa", allowBase: true, wantMalformed: true},
		{name: "trailing separator", raw: "bob@", allowBase: true, wantMalformed: true},
		{name: "multiple separators", raw: "bob@qa@extra", allowBase: true, wantMalformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handle, session, err := parseReplyToRoute(test.raw, test.allowBase)
			if test.wantMalformed {
				if err == nil || !strings.Contains(err.Error(), "malformed") {
					t.Fatalf("parseReplyToRoute(%q) error = %v, want malformed refusal", test.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReplyToRoute(%q): %v", test.raw, err)
			}
			if handle != test.wantHandle || session != test.wantSession {
				t.Fatalf(
					"parseReplyToRoute(%q) = (%q, %q), want (%q, %q)",
					test.raw,
					handle,
					session,
					test.wantHandle,
					test.wantSession,
				)
			}
		})
	}
}

func TestRouteExplainRejectsSymlinkedSessionRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	clearSendMailboxTestEnv(t)

	t.Run("same tree", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), ".agent-mail")
		sourceRoot := filepath.Join(base, "dev")
		outsideTarget := filepath.Join(t.TempDir(), "qa")
		ensureRouteAgents(t, sourceRoot, "alice")
		ensureRouteAgents(t, outsideTarget, "bob")
		if err := os.Symlink(outsideTarget, filepath.Join(base, "qa")); err != nil {
			t.Fatal(err)
		}

		result := runRouteExplainJSONForTest(t,
			"--from-root", sourceRoot,
			"--me", "alice",
			"--to", "bob",
			"--session", "qa",
		)
		if result.Routable {
			t.Fatalf("symlinked same-tree session was routable: %#v", result)
		}
		if !strings.Contains(result.Error, "direct directory under base") {
			t.Fatalf("error = %q, want direct child refusal", result.Error)
		}
	})

	t.Run("peer", func(t *testing.T) {
		sourceProjectDir := filepath.Join(t.TempDir(), "source")
		sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "dev")
		peerBase := filepath.Join(t.TempDir(), "peer-mail")
		outsideTarget := filepath.Join(t.TempDir(), "peer-qa")
		ensureRouteAgents(t, sourceRoot, "alice")
		ensureRouteAgents(t, outsideTarget, "bob")
		if err := os.MkdirAll(peerBase, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideTarget, filepath.Join(peerBase, "qa")); err != nil {
			t.Fatal(err)
		}
		writeRouteAmqrc(t, sourceProjectDir, map[string]any{
			"root":    ".agent-mail",
			"project": "source",
			"peers": map[string]string{
				"peer": peerBase,
			},
		})
		resetAmqrcCache()

		result := runRouteExplainJSONForTest(t,
			"--from-root", sourceRoot,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--session", "qa",
		)
		if result.Routable {
			t.Fatalf("symlinked peer session was routable: %#v", result)
		}
		if !strings.Contains(result.Error, "direct directory under base") {
			t.Fatalf("error = %q, want direct child refusal", result.Error)
		}
	})
}

func TestRouteExplainAndReplyRejectIncompletePeerMailbox(t *testing.T) {
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source")
	sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "collab")
	peerProjectDir := filepath.Join(t.TempDir(), "peer")
	peerBase := filepath.Join(peerProjectDir, ".agent-mail")
	peerRoot := filepath.Join(peerBase, "collab")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, peerRoot, "bob")
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	peerMissing := fsq.AgentDLQCur(peerRoot, "bob")
	if err := os.Remove(peerMissing); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer",
	)
	if result.Routable {
		t.Fatalf("incomplete peer mailbox was routable: %#v", result)
	}
	if !strings.Contains(result.Error, "incomplete") || !strings.Contains(result.Error, "dlq/cur") {
		t.Fatalf("route error = %q, want complete mailbox refusal", result.Error)
	}
	if _, err := os.Lstat(peerMissing); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired peer mailbox: %v", err)
	}

	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "p2p/peer:collab:bob__source:collab:alice",
		ReplyTo:      "bob@collab",
		ReplyProject: "peer",
		FromProject:  "peer",
	})
	_, _, replyErr := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--body", "must not deliver",
		})
	})
	if replyErr == nil {
		t.Fatal("reply to incomplete peer mailbox succeeded")
	}
	if !strings.Contains(replyErr.Error(), "incomplete") || !strings.Contains(replyErr.Error(), "dlq/cur") {
		t.Fatalf("reply error = %v, want complete mailbox refusal", replyErr)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("peer inbox count = %d, want 0", got)
	}
	if _, err := os.Lstat(peerMissing); !os.IsNotExist(err) {
		t.Fatalf("reply repaired peer mailbox: %v", err)
	}
}

func TestReplyRejectsSymlinkedPeerSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source")
	sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "collab")
	peerBase := filepath.Join(t.TempDir(), "peer-mail")
	outsideTarget := filepath.Join(t.TempDir(), "peer-qa")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, outsideTarget, "bob")
	if err := os.MkdirAll(peerBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, filepath.Join(peerBase, "qa")); err != nil {
		t.Fatal(err)
	}
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "federated-thread",
		ReplyTo:      "bob@qa",
		ReplyProject: "peer",
		FromProject:  "peer",
	})
	_, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--body", "must not escape peer base",
		})
	})
	if err == nil {
		t.Fatal("reply through symlinked peer session succeeded")
	}
	if !strings.Contains(err.Error(), "direct directory under base") {
		t.Fatalf("error = %v, want direct child refusal", err)
	}
	if got := inboxCount(t, outsideTarget, "bob"); got != 0 {
		t.Fatalf("outside peer inbox count = %d, want 0", got)
	}
}

func deliverOriginalForReply(t *testing.T, root, recipient string, header format.Header) string {
	t.Helper()
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	header.Schema = format.CurrentSchema
	header.ID = id
	header.Created = now.UTC().Format(time.RFC3339Nano)
	message := format.Message{Header: header, Body: "request"}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxesForTest(t, root, []string{recipient}, id+".md", data); err != nil {
		t.Fatal(err)
	}
	return id
}

func soleDeliveredMessage(t *testing.T, root, agent string) format.Message {
	t.Helper()
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, agent))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	message, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, agent), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	return message
}
