package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupWritesRemoteHarnessAndSnapshot(t *testing.T) {
	home, root, _ := pinInstallShell(t)
	out := filepath.Join(t.TempDir(), agentSnapshotName)
	stdout := captureStdout(t, func() int {
		return run([]string{"setup", "--out", out})
	})
	want := "In Buzz Desktop: Agents, then + then Import, pick " + out + ", then Start. Then run /amq-remote in a session.\n"
	if stdout != want {
		t.Fatalf("stdout = %q", stdout)
	}

	info, err := os.Stat(harnessPath(home, remoteHarnessID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("harness mode = %o", info.Mode().Perm())
	}
	harness := readHarness(t, harnessPath(home, remoteHarnessID+".json"))
	exe, err := currentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if harness.ID != remoteHarnessID || harness.Command != exe || len(harness.Args) != 0 {
		t.Fatalf("harness id=%q command=%q args=%#v", harness.ID, harness.Command, harness.Args)
	}
	if len(harness.Env) != 1 || harness.Env["AMQ_ACP_REMOTE"] != "binding" {
		t.Fatalf("env = %#v, want only AMQ_ACP_REMOTE=binding (root %s must stay out)", harness.Env, root)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot agentSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Format != "buzz-agent-snapshot" || snapshot.Version != 1 ||
		snapshot.Definition.Name != "AMQ Remote" || snapshot.Definition.Runtime != remoteHarnessID ||
		snapshot.Definition.Model != remoteModelID || snapshot.Definition.Parallelism != 1 ||
		snapshot.Definition.RespondTo != ownerOnlyRespondTo ||
		snapshot.Profile.DisplayName != "AMQ Remote" || snapshot.Memory.Level != "none" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
