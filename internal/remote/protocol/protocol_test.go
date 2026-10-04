package protocol

import (
	"encoding/json"
	"testing"
)

// corpusFile is the shared contract corpus every remote component consumes.
const corpusFile = "../../../testdata/remote/corpus.json"

// TestDecodeCorpusCommands is the happy path: every client command in the
// corpus, once the fixture defaults are filled in, decodes without error.
// Clause 6 (611.22.19 round-2): the test also verifies fillCommand semantic
// equality — the decoded command must preserve every VALUE (strings, arrays,
// numbers, nested objects like input.min_evidence) from the filled wire
// command, ignoring key order and escape spelling only. This is the .4
// closure condition: fillCommand is the single source of truth for the
// corpus round-trip contract.
func TestDecodeCorpusCommands(t *testing.T) {
	defaults, fixtures := loadCorpus(t)
	for _, f := range fixtures {
		for i, step := range f.Steps {
			if step.Client == nil {
				continue
			}
			cmd := fillCommand(step.Client, defaults)
			data, err := json.Marshal(cmd)
			if err != nil {
				t.Fatalf("%s step %d: marshal: %v", f.ID, i, err)
			}
			decoded, err := DecodeCommand(data)
			if err != nil {
				t.Fatalf("%s step %d: decode %s: %v", f.ID, i, data, err)
			}
			op := Op(cmd["op"].(string))
			if decoded.Op != op {
				t.Fatalf("%s step %d: op %q != %q", f.ID, i, decoded.Op, op)
			}
			// Semantic equality: re-serialize the decoded command and compare
			// VALUES against the filled wire command. Key order and escape
			// spelling may differ; the parsed value tree must match.
			reData, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("%s step %d: re-marshal: %v", f.ID, i, err)
			}
			var reDoc map[string]any
			if err := json.Unmarshal(reData, &reDoc); err != nil {
				t.Fatalf("%s step %d: re-decode: %v", f.ID, i, err)
			}
			if !semanticEqual(cmd, reDoc) {
				t.Fatalf("%s step %d: fillCommand round-trip lost a value\nfilled: %s\ndecoded: %s",
					f.ID, i, mustJSON(cmd), mustJSON(reDoc))
			}
		}
	}
}

func setDefault(m map[string]any, key, value string) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

// semanticEqual compares two parsed-JSON value trees for semantic equality:
// strings, bools, numbers, arrays, and nested objects must match by value.
// Key order and escape spelling are ignored (both are already parsed). A
// float64 from json.Unmarshal is compared to the int/float source by value.
func semanticEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !semanticEqual(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !semanticEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case float64:
		// json.Unmarshal always produces float64 for numbers; the filled
		// command may have an int. Compare by value.
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int:
			return av == float64(bv)
		default:
			return false
		}
	default:
		return a == b
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
