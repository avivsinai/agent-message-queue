package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

func TestBuzzHarnessIsPreviewBYOH(t *testing.T) {
	raw, err := os.ReadFile("buzz-harness.json")
	if err != nil {
		t.Fatal(err)
	}

	var harness struct {
		ID      string            `json:"id"`
		Label   string            `json:"label"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &harness); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]*$`).MatchString(harness.ID) {
		t.Fatalf("id %q is not a Buzz custom harness id", harness.ID)
	}
	if harness.Command != "amq-acp" {
		t.Fatalf("command = %q, want amq-acp", harness.Command)
	}
	if len(harness.Args) != 0 {
		t.Fatalf("args = %#v, want empty (Buzz default acp argv is a usage error)", harness.Args)
	}
	if len(harness.Env) != 0 {
		t.Fatalf("env = %#v, want omitted so a committed file cannot ship a root", harness.Env)
	}
}
