package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// TestCommandDigestResolvesDefaults reproduces Pro F3: the digest was a
// function of SPELLING, not meaning. SubmitInput.Busy/Deliver are omitempty,
// so {"text":"say hi"} (omitted) and {"text":"say hi","busy":"reject","deliver":"turn"}
// (spelled) produced DIFFERENT digests — request_conflict for an identical
// retry. The CLI always spells the defaults; a mailbox peer may omit both.
// FIX: resolve defaults BEFORE digesting so both forms produce the same digest.
func TestCommandDigestResolvesDefaults(t *testing.T) {
	base := &Command{
		Schema:    SchemaRequest,
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

// TestCommandDigestNormalizesNotAfter reproduces B1: the digest was a
// function of SPELLING, not meaning, for not_after. RFC3339 allows many legal
// spellings of the same instant (Z, +00:00, .000Z, +02:00), and the RAW STRING
// went into the digest. A non-Go carrier that parses and re-emits the deadline
// (Python isoformat() gives +00:00, JS toISOString() gives .000Z) gets
// request_conflict on an identical retry. FIX: normalize via
// FormatTime(ParseTime(NotAfter)) before digesting — the digest is a function
// of MEANING, not spelling.
func TestCommandDigestNormalizesNotAfter(t *testing.T) {
	base := &Command{
		Schema:    SchemaRequest,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-1111111119b1",
		TargetID:  "fake",
		Epoch:     "e_1",
		Input:     &SubmitInput{Text: "say hi"},
	}
	spellings := []string{
		"2026-09-08T10:02:00Z",
		"2026-09-08T10:02:00.000Z",
		"2026-09-08T10:02:00+00:00",
		"2026-09-08T12:02:00+02:00",
	}
	digests := make(map[string]struct{})
	for _, s := range spellings {
		cmd := *base
		cmd.NotAfter = s
		d := CommandDigest(&cmd)
		if d == "" {
			t.Fatalf("empty digest for not_after=%q", s)
		}
		digests[d] = struct{}{}
	}
	if len(digests) != 1 {
		t.Fatalf("not_after spellings of the same instant produced %d different digests (B1 — the digest is a function of spelling, not meaning): %v", len(digests), digests)
	}
}

// TestCommandDigestHTMLEscaping reproduces Pro F1: a foreign carrier following
// the byte template (no HTML escaping) computes a different digest than Go's
// json.Marshal for input containing <, >, or &. The canonical form is EXACTLY
// json.Marshal's output.
func TestCommandDigestHTMLEscaping(t *testing.T) {
	cmd := &Command{
		Schema:    SchemaRequest,
		Op:        OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111902",
		TargetID:  "fake",
		Epoch:     "e_1",
		NotAfter:  "2026-09-12T12:00:00Z",
		Input:     &SubmitInput{Text: "fix the <div> & ship"},
	}
	got := CommandDigest(cmd)
	// Build the expected digest from json.Marshal (with resolved defaults).
	payload := digestPayload{
		Schema: cmd.Schema, Op: cmd.Op, RequestID: cmd.RequestID,
		TargetID: cmd.TargetID, Epoch: cmd.Epoch, NotAfter: normalizeNotAfter(cmd.NotAfter),
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
