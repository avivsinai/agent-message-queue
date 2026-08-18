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

	prepare.Target.ProjectRoot = root
	prepare.Placement = &PlacementV1{Target: PlacementSession, Layout: PlacementColumns, LauncherPane: "%1"}
	raw, _ = json.Marshal(prepare)
	if _, err := DecodePrepareRequestV1(raw); err == nil || !strings.Contains(err.Error(), "launcher_pane") {
		t.Fatalf("session launcher_pane error = %v", err)
	}
	hostilePlacement := strings.Replace(string(raw), `"launcher_pane":"%1"`, `"launcher_pane":"%1","window":"@1"`, 1)
	if _, err := DecodePrepareRequestV1([]byte(hostilePlacement)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("hostile placement error = %v", err)
	}
}

func TestDecodeInspectRequestRejectsInvalidUTF8(t *testing.T) {
	raw := append([]byte(`{"request_version":1,"target":{"project_root":"`), 0xff)
	raw = append(raw, []byte(`","base_root":"","session_root":"/tmp/session","session":"collab"}}`)...)
	if _, err := DecodeInspectRequestV1(raw); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("DecodeInspectRequestV1 error = %v", err)
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
	request.SubjectSchema = 3
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "subject schema") {
		t.Fatalf("invalid subject schema error = %v", err)
	}
	request.SubjectSchema = SubjectSchemaV2

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

func TestCallerContextJSONRejectsDuplicateNullInvalidAndUnknownAdjacentFields(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	intent, err := DecodeLaunchIntentV1([]byte(validIntentJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	request := PrepareRequestV1{
		RequestVersion: 1,
		Target:         TargetV1{ProjectRoot: root, SessionRoot: filepath.Join(root, ".agent-mail", "collab"), Session: "collab"},
		Launcher:       "commands", CallerContext: map[string]string{"run_id": "run-42"}, Intent: intent,
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "duplicate", raw: []byte(strings.Replace(string(raw), `"run_id":"run-42"`, `"run_id":"run-42","run_id":"other"`, 1)), want: "duplicate key"},
		{name: "null", raw: []byte(strings.Replace(string(raw), `{"run_id":"run-42"}`, `null`, 1)), want: "must be an object"},
		{name: "unknown adjacent", raw: []byte(strings.Replace(string(raw), `"caller_context":`, `"caller_context_policy":"opaque","caller_context":`, 1)), want: "unknown field"},
		{name: "invalid UTF-8", raw: append(append([]byte(nil), raw[:len(raw)-1]...), 0xff, '}'), want: "invalid UTF-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodePrepareRequestV1(test.raw); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodePrepareRequestV1 error = %v, want %q", err, test.want)
			}
		})
	}
}
