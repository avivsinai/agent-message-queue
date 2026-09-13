package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenDigestVectors loads testdata/remote/digest_vectors.json and
// asserts that CommandDigest reproduces every expected digest. The vectors
// pin the exact bytes a cross-language carrier must produce: plain text,
// HTML-escaping (< > &), busy/deliver omitted vs spelled, and not_after
// normalization (Z, .000Z, +02:00, sub-second trailing-zero trim).
//
// The vectors are the EXECUTABLE contract — prose drifts, vectors do not.
// A foreign carrier runs the same JSON file and proves it agrees without
// reading a word of Go.
func TestGoldenDigestVectors(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", "..", "testdata", "remote", "digest_vectors.json"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Description string `json:"description"`
		Vectors     []struct {
			Name      string  `json:"name"`
			Command   Command `json:"command"`
			ExpectSHA string  `json:"expect_sha256"`
			Note      string  `json:"note,omitempty"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Vectors) < 7 {
		t.Fatalf("expected at least 7 vectors, got %d", len(doc.Vectors))
	}
	for _, v := range doc.Vectors {
		got := CommandDigest(&v.Command)
		if got != v.ExpectSHA {
			t.Fatalf("vector %q: CommandDigest = %s, want %s (golden vector drift — the code and the published vectors disagree)", v.Name, got, v.ExpectSHA)
		}
	}
	t.Logf("verified %d golden digest vectors", len(doc.Vectors))
}
