package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// The owner turns a session's mirror off and on again from the CLI.
func TestMirrorOffThenOnFromTheCLI(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: dir, Handle: "one", Name: "one", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ verb, want string }{{"off", "off"}, {"status", "off"}, {"on", "on"}} {
		var out bytes.Buffer
		if code := run([]string{"mirror", step.verb, "--name", "one", "--json"}, nil, &out, io.Discard); code != 0 {
			t.Fatalf("mirror %s: exit %d: %s", step.verb, code, out.String())
		}
		var got struct {
			Sessions []struct {
				Mirror string `json:"mirror"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Sessions) != 1 || got.Sessions[0].Mirror != step.want {
			t.Fatalf("mirror %s: output %q (%v); want mirror %q", step.verb, out.String(), err, step.want)
		}
	}
}
