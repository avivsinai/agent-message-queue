package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
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
	seen := 0
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

// TestCommandDigestResolvesDefaults reproduces Pro F3: the digest was a
// function of SPELLING, not meaning. SubmitInput.Busy/Deliver are omitempty,
// so {"text":"say hi"} (omitted) and {"text":"say hi","busy":"reject","deliver":"turn"}
// (spelled) produced DIFFERENT digests — request_conflict for an identical
// retry. The CLI always spells the defaults; a mailbox peer may omit both.
// FIX: resolve defaults BEFORE digesting so both forms produce the same digest.
func TestCommandDigestResolvesDefaults(t *testing.T) {
	base := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111901",
		TargetID:  "fake",
		Epoch:     "e_1",
		NotAfter:  "2026-09-12T12:00:00Z",
	}
	omitted := *base
	omitted.Input = &SubmitInput{Text: "say hi"} // Busy="", Deliver=""

	spelled := *base
	spelled.Input = &SubmitInput{Text: "say hi", Busy: BusyReject, Deliver: DeliverTurn}

	dOmitted := CommandDigest(&omitted)
	dSpelled := CommandDigest(&spelled)
	if dOmitted != dSpelled {
		t.Fatalf("digest of omitted form (%s) != spelled form (%s) — the digest is a function of spelling, not meaning (Pro F3)", dOmitted, dSpelled)
	}
	if dOmitted == "" {
		t.Fatal("digest is empty")
	}
}

// TestCommandDigestExcludesNotAfter reproduces B10: NotAfter is NOT in the
// digest. A deadline is POLICY about the request, not its identity. A retry
// with a fresh deadline (the normal case) must not change the digest and hit
// request_conflict. Including NotAfter meant the digest was a function of
// the deadline spelling/value — removing it entirely is the correct fix.
func TestCommandDigestExcludesNotAfter(t *testing.T) {
	base := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-1111111119b1",
		TargetID:  "fake",
		Epoch:     "e_1",
		Input:     &SubmitInput{Text: "say hi"},
	}
	// Different deadlines, same identity -> same digest.
	deadlines := []string{
		"2026-09-08T10:02:00Z",
		"2026-09-08T10:02:00.000Z",
		"2026-09-08T10:02:00+00:00",
		"2026-09-08T12:02:00+02:00",
		"2026-09-09T00:00:00Z",
		"", // no deadline at all
	}
	digests := make(map[string]struct{})
	for _, s := range deadlines {
		cmd := *base
		cmd.NotAfter = s
		d := CommandDigest(&cmd)
		if d == "" {
			t.Fatalf("empty digest for not_after=%q", s)
		}
		digests[d] = struct{}{}
	}
	if len(digests) != 1 {
		t.Fatalf("different deadlines produced %d different digests (B10 — NotAfter must not be in the digest): %v", len(digests), digests)
	}
}

// TestCommandDigestHTMLEscaping reproduces Pro F1: a foreign carrier following
// the byte template (no HTML escaping) computes a different digest than Go's
// json.Marshal for input containing <, >, or &. The canonical form is EXACTLY
// json.Marshal's output.
func TestCommandDigestHTMLEscaping(t *testing.T) {
	cmd := &Command{
		Schema:    SchemaCommand,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111902",
		TargetID:  "fake",
		Epoch:     "e_1",
		NotAfter:  "2026-09-12T12:00:00Z",
		Input:     &SubmitInput{Text: "fix the <div> & ship"},
	}
	got := CommandDigest(cmd)
	// Build the expected digest from json.Marshal (with resolved defaults).
	// B10: NotAfter is NOT in the digestPayload.
	payload := digestPayload{
		Schema: cmd.Schema, Op: cmd.Op, RequestID: cmd.RequestID,
		TargetID: cmd.TargetID, Epoch: cmd.Epoch,
		Input: resolveDigestDefaults(cmd.Input),
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	want := digestPrefix + hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("digest mismatch for HTML-escaping input: got %s, want %s (Pro F1 — canonical form must be json.Marshal's output)", got, want)
	}
	// Verify the marshalled data actually contains escaped sequences (proves
	// the test is exercising the escape path).
	if !bytes.Contains(data, []byte("\\u003c")) && !bytes.Contains(data, []byte("&lt;")) {
		// Go's json.Marshal escapes < as \u003c by default.
		t.Fatalf("marshalled data does not contain HTML-escaped sequences: %s", data)
	}
}
