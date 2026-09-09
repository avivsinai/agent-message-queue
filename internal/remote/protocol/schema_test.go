package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaDir is the repository schemas directory, resolved from the package
// source directory (internal/remote/protocol -> ../../../schemas), matching the
// corpusFile relative path used by the other tests in this package.
var schemaDir = filepath.Clean(filepath.Join("..", "..", "..", "schemas"))

// compileSchema compiles one of the frozen v1 schemas. The default file loader
// resolves the schema's internal #/$defs refs; AddResource is not needed because
// the schemas are self-contained.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	sch, err := c.Compile(filepath.Join(schemaDir, name))
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return sch
}

// corpusFixture is the slice of the shared corpus the schema test consumes.
type corpusFixture struct {
	ID    string             `json:"id"`
	Steps []corpusStep       `json:"steps"`
}
type corpusStep struct {
	Client map[string]any `json:"client,omitempty"`
	Reply  map[string]any `json:"reply,omitempty"`
	Expect map[string]any `json:"expect,omitempty"`
}

// loadCorpus reads the shared contract corpus and its defaults.
func loadCorpus(t *testing.T) (defaults map[string]string, fixtures []corpusFixture) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(corpusFile))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Defaults  map[string]string `json:"defaults"`
		Fixtures  []corpusFixture   `json:"fixtures"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	return c.Defaults, c.Fixtures
}

// fillCommand applies the corpus defaults to one client command the same way
// TestDecodeCorpusCommands does, so the document is a complete wire command
// ready for schema validation (not just Go decoding).
func fillCommand(cmd map[string]any, defaults map[string]string) map[string]any {
	out := map[string]any{"schema": SchemaCommand}
	for k, v := range cmd {
		out[k] = v
	}
	host := defaults["creator_host"]
	if h, ok := out["creator_host"].(string); ok {
		host = h
		delete(out, "creator_host")
	}
	if id, ok := out["request_ref_for"].(string); ok {
		out["request_ref"] = EncodeRef(host, defaults["target_id"], id)
		delete(out, "request_ref_for")
	}
	op := Op(out["op"].(string))
	switch op {
	case OpRequestSubmit, OpRequestCancel, OpInteractionRespond:
		setDefault(out, "target_id", defaults["target_id"])
		setDefault(out, "epoch", defaults["epoch"])
		if op != OpInteractionRespond {
			setDefault(out, "not_after", defaults["not_after"])
		}
	case OpSessionInspect, OpSessionEvents:
		setDefault(out, "target_id", defaults["target_id"])
	}
	return out
}

// TestCorpusCommandsValidateAgainstSchema is the B12 guard: every client
// command in the shared corpus, once defaults are filled, validates against
// schemas/remote-command-v1.schema.json. Before the oneOf-branch schema fix
// this was 0/34 because each branch required `schema` but did not list it as a
// property under additionalProperties:false.
func TestCorpusCommandsValidateAgainstSchema(t *testing.T) {
	defaults, fixtures := loadCorpus(t)
	sch := compileSchema(t, "remote-command-v1.schema.json")
	seen := 0
	for _, f := range fixtures {
		for i, step := range f.Steps {
			if step.Client == nil {
				continue
			}
			cmd := fillCommand(step.Client, defaults)
			if err := sch.Validate(cmd); err != nil {
				t.Fatalf("%s step %d: command does not validate against schema: %v\n%s",
					f.ID, i, err, mustJSON(cmd))
			}
			seen++
		}
	}
	if seen < 20 {
		t.Fatalf("corpus exercised only %d client commands", seen)
	}
	t.Logf("validated %d corpus commands against remote-command-v1.schema.json", seen)
}

// TestRequestSnapshotsValidateAgainstSchema covers the request-snapshot schema:
// a full completed record, the compacted-completed tombstone (completed with
// result_expired code and no result), and a plain running record all validate.
// The compacted variant is the B12 addition: a tombstoned completed record
// keeps state=completed + code=result_expired instead of carrying a result.
func TestRequestSnapshotsValidateAgainstSchema(t *testing.T) {
	sch := compileSchema(t, "remote-request-v1.schema.json")
	ref := EncodeRef("hostA", "t_fake1", "11111111-1111-4111-8111-111111111101")
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		doc  map[string]any
	}{
		{"running", map[string]any{
			"schema": SchemaRequest, "request_ref": ref, "request_id": "11111111-1111-4111-8111-111111111101",
			"creator_host": "hostA", "target_id": "t_fake1", "epoch": "e_1", "revision": 3,
			"state": "running", "native_run": "run_1", "observed_at": "2026-09-01T00:00:00Z",
		}},
		{"completed-full", map[string]any{
			"schema": SchemaRequest, "request_ref": ref, "request_id": "11111111-1111-4111-8111-111111111101",
			"creator_host": "hostA", "target_id": "t_fake1", "epoch": "e_1", "revision": 4,
			"state": "completed", "native_run": "run_1",
			"result": map[string]any{"text": "hi", "truncated": false},
			"observed_at": "2026-09-01T00:00:00Z",
		}},
		{"completed-compacted", map[string]any{
			"schema": SchemaRequest, "request_ref": ref, "request_id": "11111111-1111-4111-8111-111111111101",
			"creator_host": "hostA", "target_id": "t_fake1", "epoch": "e_1", "revision": 5,
			"state": "completed", "code": "result_expired", "input_digest": digest,
			"observed_at": "2026-09-01T00:00:00Z",
		}},
		{"failed", map[string]any{
			"schema": SchemaRequest, "request_ref": ref, "request_id": "11111111-1111-4111-8111-111111111101",
			"creator_host": "hostA", "target_id": "t_fake1", "epoch": "e_1", "revision": 4,
			"state": "failed", "code": "native_error", "native_run": "run_1",
			"observed_at": "2026-09-01T00:00:00Z",
		}},
	}
	for _, c := range cases {
		if err := sch.Validate(c.doc); err != nil {
			t.Fatalf("%s: snapshot does not validate: %v\n%s", c.name, err, mustJSON(c.doc))
		}
	}

	// A compacted completed record carrying the WRONG code must be rejected:
	// the only code admitted on a completed tombstone is result_expired.
	bad := map[string]any{
		"schema": SchemaRequest, "request_ref": ref, "request_id": "11111111-1111-4111-8111-111111111101",
		"creator_host": "hostA", "target_id": "t_fake1", "epoch": "e_1", "revision": 5,
		"state": "completed", "code": "native_error", "input_digest": digest,
		"observed_at": "2026-09-01T00:00:00Z",
	}
	if err := sch.Validate(bad); err == nil {
		t.Fatalf("completed tombstone with non-result_expired code was accepted")
	}
}

// TestTruncateTextIsUTF8Safe pins the B08 truncation contract: the result is at
// most max bytes, no multi-byte rune is split, and truncation is reported only
// when the input exceeded the bound.
func TestTruncateTextIsUTF8Safe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"under-limit", "abc", 10},
		{"exact-limit", "abc", 3},
		{"ascii-trim", "abcdef", 3},
		{"rune-split-mid", "héllo", 2}, // é is 2 bytes; max=2 lands mid-rune
		{"rune-split-after", "héllo", 3},
	}
	for _, c := range cases {
		got, trunc := TruncateText(c.in, c.max)
		if len(got) > c.max {
			t.Fatalf("%s: result %q (%d bytes) exceeds max %d", c.name, got, len(got), c.max)
		}
		if !utf8ValidString(got) {
			t.Fatalf("%s: result %q is not valid UTF-8", c.name, got)
		}
		wantTrunc := len(c.in) > c.max
		if trunc != wantTrunc {
			t.Fatalf("%s: trunc=%v want=%v", c.name, trunc, wantTrunc)
		}
	}
	// Large input with a trailing multi-byte rune split at the boundary.
	big := strings.Repeat("x", MaxResultBytes-1) + "é" // é is 2 bytes; total = MaxResultBytes+1
	got, trunc := TruncateText(big, MaxResultBytes)
	if len(got) > MaxResultBytes || !utf8ValidString(got) || !trunc {
		t.Fatalf("large rune-split: len=%d valid=%v trunc=%v", len(got), utf8ValidString(got), trunc)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// utf8ValidString mirrors unicode/utf8.ValidString without importing the
// package here (the protocol package keeps a minimal test-only helper).
func utf8ValidString(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		var size int
		switch {
		case c < 0x80:
			size = 1
		case c&0xE0 == 0xC0:
			size = 2
		case c&0xF0 == 0xE0:
			size = 3
		case c&0xF8 == 0xF0:
			size = 4
		default:
			return false
		}
		if i+size > len(s) {
			return false
		}
		for j := 1; j < size; j++ {
			if s[i+j]&0xC0 != 0x80 {
				return false
			}
		}
		i += size
	}
	return true
}
