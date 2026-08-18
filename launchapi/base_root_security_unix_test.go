//go:build !windows

package launchapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareRejectsWritableProjectAMQRCWithoutWrites(t *testing.T) {
	fixture := newPublicPrepareFixture(t, false)
	root, err := filepath.EvalSymlinks(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(fixture.request.Target.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(root, "configured")
	if err := os.Mkdir(configured, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{"root": configured})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(project, ".amqrc")
	if err := os.WriteFile(configPath, append(data, '\n'), 0o622); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o622); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(configured, "profile-a")
	fixture.request.Target = TargetV1{
		ProjectRoot: project, BaseRoot: profile,
		SessionRoot: filepath.Join(profile, "collab"), Session: "collab",
	}
	fixture.request.Intent.Participants = []ParticipantV1{{Handle: "operator", Runnable: false}}
	before := snapshotTestTree(t, fixture.root)
	result, err := Prepare(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != PrepareOutcomeUnsupported || result.Reason != "base_root_unauthorized" {
		t.Fatalf("writable .amqrc refusal = %#v", result)
	}
	if len(result.PlannedWrites) != 0 || snapshotTestTree(t, fixture.root) != before {
		t.Fatalf("writable .amqrc refusal mutated or planned writes: %#v", result)
	}
}
