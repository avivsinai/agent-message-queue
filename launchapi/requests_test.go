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

func TestDecodeInspectRequestV1RejectsInvalidUTF8(t *testing.T) {
	raw := append([]byte(`{"request_version":1,"target":{"project_root":"`), 0xff)
	raw = append(raw, []byte(`","base_root":"","session_root":"/tmp/session","session":"collab"}}`)...)
	if _, err := DecodeInspectRequestV1(raw); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("DecodeInspectRequestV1 error = %v", err)
	}
}
