package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusFile is the shared contract corpus every remote component consumes.
const corpusFile = "../../../testdata/remote/corpus.json"

type corpus struct {
	Defaults struct {
		TargetID    string `json:"target_id"`
		Epoch       string `json:"epoch"`
		CreatorHost string `json:"creator_host"`
		NotAfter    string `json:"not_after"`
	} `json:"defaults"`
	Fixtures []struct {
		ID    string           `json:"id"`
		Steps []map[string]any `json:"steps"`
	} `json:"fixtures"`
}

// TestDecodeCorpusCommands is the happy path: every client command in the
// corpus, once the fixture defaults are filled in, decodes without error.
func TestDecodeCorpusCommands(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(corpusFile))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	seen := 0
	for _, f := range c.Fixtures {
		for i, step := range f.Steps {
			raw, ok := step["client"].(map[string]any)
			if !ok {
				continue
			}
			cmd := map[string]any{"schema": SchemaCommand}
			for k, v := range raw {
				cmd[k] = v
			}
			host := c.Defaults.CreatorHost
			if h, ok := cmd["creator_host"].(string); ok {
				host = h
				delete(cmd, "creator_host")
			}
			if id, ok := cmd["request_ref_for"].(string); ok {
				cmd["request_ref"] = EncodeRef(host, c.Defaults.TargetID, id)
				delete(cmd, "request_ref_for")
			}
			op := Op(cmd["op"].(string))
			switch op {
			case OpRequestSubmit, OpRequestCancel, OpInteractionRespond:
				setDefault(cmd, "target_id", c.Defaults.TargetID)
				setDefault(cmd, "epoch", c.Defaults.Epoch)
				if op != OpInteractionRespond {
					setDefault(cmd, "not_after", c.Defaults.NotAfter)
				}
			case OpSessionInspect, OpSessionEvents:
				setDefault(cmd, "target_id", c.Defaults.TargetID)
			}
			data, err := json.Marshal(cmd)
			if err != nil {
				t.Fatalf("%s step %d: marshal: %v", f.ID, i, err)
			}
			decoded, err := DecodeCommand(data)
			if err != nil {
				t.Fatalf("%s step %d: decode %s: %v", f.ID, i, data, err)
			}
			if decoded.Op != op {
				t.Fatalf("%s step %d: op %q != %q", f.ID, i, decoded.Op, op)
			}
			seen++
		}
	}
	if seen < 20 {
		t.Fatalf("corpus exercised only %d client commands", seen)
	}
}

func setDefault(m map[string]any, key, value string) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

func TestRequestRefRoundTrip(t *testing.T) {
	ref := EncodeRef("hostA", "t_fake1", "11111111-1111-4111-8111-111111111101")
	if !strings.HasPrefix(ref, RefPrefix) {
		t.Fatalf("ref %q lacks prefix", ref)
	}
	host, target, id, err := DecodeRef(ref)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if host != "hostA" || target != "t_fake1" || id != "11111111-1111-4111-8111-111111111101" {
		t.Fatalf("round trip mismatch: %s %s %s", host, target, id)
	}
}

// TestDecodeRefusesDuplicateAndUnknownKeys pins the strictness the design
// requires: two carriers must never disagree about the same bytes.
func TestDecodeRefusesDuplicateAndUnknownKeys(t *testing.T) {
	base := `{"schema":"amq.remote.command/1","op":"session.list"`
	for name, doc := range map[string]string{
		"duplicate": base + `,"op":"session.list"}`,
		"unknown":   base + `,"extra":1}`,
		"trailing":  base + `}{}`,
	} {
		_, err := DecodeCommand([]byte(doc))
		if ExitCode(err) != ExitUsage {
			t.Fatalf("%s: want usage refusal, got %v", name, err)
		}
	}
	if _, err := DecodeCommand([]byte(base + "}")); err != nil {
		t.Fatalf("clean document refused: %v", err)
	}
}

// TestValidateRejectsWhitespaceOnlyPrompt pins the shared rule for
// agent-message-queue-611.22.28: a whitespace-only prompt is empty. The CLI
// refused it, but every other carrier (the AMQ mailbox, a Buzz DM) reaches
// the harness through Validate alone, and Validate checked only Text == "".
// A command carrying "   " was dispatched to a real coding session.
func TestValidateRejectsWhitespaceOnlyPrompt(t *testing.T) {
	for _, text := range []string{"   ", "\t", "\n", " \t\n "} {
		cmd := &Command{
			Schema:    SchemaCommand,
			Op:        OpRequestSubmit,
			RequestID: "11111111-1111-4111-8111-111111111502",
			TargetID:  "t_fake1",
			Epoch:     "e_1",
			NotAfter:  "2026-09-08T10:02:00Z",
			Input:     &SubmitInput{Text: text},
		}
		if err := cmd.Validate(); err == nil {
			t.Fatalf("Validate accepted a whitespace-only prompt %q", text)
		}
	}
	// A prompt with real content and surrounding whitespace is still valid.
	cmd := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111503",
		TargetID:  "t_fake1",
		Epoch:     "e_1",
		NotAfter:  "2026-09-08T10:02:00Z",
		Input:     &SubmitInput{Text: "  do the thing  "},
	}
	if err := cmd.Validate(); err != nil {
		t.Fatalf("Validate rejected a padded but non-empty prompt: %v", err)
	}
}
