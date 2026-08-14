package launchapi

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicRequestCodecsRejectUnknownAndMalformedAuthority(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	intent, err := DecodeLaunchIntentV1([]byte(validIntentJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	prepare := PrepareRequestV1{
		RequestVersion: 1,
		Target:         TargetV1{ProjectRoot: root, SessionRoot: filepath.Join(root, ".agent-mail", "collab"), Session: "collab"},
		Launcher:       "auto",
		Intent:         intent,
	}
	raw, err := json.Marshal(prepare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePrepareRequestV1(raw); err != nil {
		t.Fatal(err)
	}
	hostile := strings.Replace(string(raw), `"launcher":"auto"`, `"launcher":"auto","binding":{"resource":"foreign"}`, 1)
	if _, err := DecodePrepareRequestV1([]byte(hostile)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("hostile Prepare error = %v", err)
	}
	missingRunnable := strings.Replace(string(raw), `"runnable":false`, `"runnable_omitted":false`, 1)
	if _, err := DecodePrepareRequestV1([]byte(missingRunnable)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("nested hostile Prepare error = %v", err)
	}

	prepare.Target.ProjectRoot = "relative"
	raw, _ = json.Marshal(prepare)
	if _, err := DecodePrepareRequestV1(raw); err == nil || !strings.Contains(err.Error(), "clean absolute") {
		t.Fatalf("relative project root error = %v", err)
	}
}

func TestApplyRequestV1RejectsDigestAndDecisionReplayShapes(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	intent, err := DecodeLaunchIntentV1([]byte(validIntentJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequestV1{
		RequestVersion: 1,
		Prepare: PrepareRequestV1{
			RequestVersion: 1,
			Target:         TargetV1{ProjectRoot: root, SessionRoot: filepath.Join(root, ".agent-mail", "collab"), Session: "collab"},
			Launcher:       "commands",
			Intent:         intent,
		},
		SubjectDigest: "sha256:" + strings.Repeat("a", 64),
		Decisions:     []DecisionV1{{ActionID: "trust:1", Choice: "trust_exact_subject"}},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}

	request.SubjectDigest = "sha256:ABC"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("invalid digest error = %v", err)
	}
	request.SubjectDigest = "sha256:" + strings.Repeat("a", 64)
	request.Decisions[0].Choice = "replay_forever"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "invalid choice") {
		t.Fatalf("invalid choice error = %v", err)
	}
	request.Decisions[0].Choice = "trust_exact_subject"
	request.Decisions = append(request.Decisions, request.Decisions[0])
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate decision") {
		t.Fatalf("duplicate decision error = %v", err)
	}

	request.Decisions = nil
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `,"decisions":null`, ``, 1))
	if _, err := DecodeApplyRequestV1(raw); err == nil || !strings.Contains(err.Error(), "requires decisions") {
		t.Fatalf("missing decisions error = %v", err)
	}
}
