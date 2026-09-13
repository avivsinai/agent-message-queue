package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenDigestVectors loads testdata/remote/digest_vectors.json and
// asserts two things:
//  1. CommandDigest reproduces every expected digest.
//  2. Vectors sharing an equivalence_group produce the SAME digest, and
//     different groups produce DIFFERENT digests.
//
// The equivalence groups are the whole point: the not_after family (Z,
// .000Z, +02:00, -00:00) shares one request_id and one instant — their
// digests MUST be identical, proving the digest is a function of MEANING,
// not spelling. The sub-second pair (.500Z, .5Z) proves the trailing-zero
// trim. Without the grouping assertion, the file can drift back to seven
// unrelated digests that all pass individually but prove nothing.
func TestGoldenDigestVectors(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", "..", "testdata", "remote", "digest_vectors.json"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Description string `json:"description"`
		Vectors     []struct {
			Name             string  `json:"name"`
			Command          Command `json:"command"`
			ExpectSHA        string  `json:"expect_sha256"`
			EquivalenceGroup string  `json:"equivalence_group,omitempty"`
			Note             string  `json:"note,omitempty"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Vectors) < 10 {
		t.Fatalf("expected at least 10 vectors, got %d", len(doc.Vectors))
	}

	// 1. Each vector reproduces its own digest.
	groupDigests := map[string]string{}
	for _, v := range doc.Vectors {
		got := CommandDigest(&v.Command)
		if got != v.ExpectSHA {
			t.Fatalf("vector %q: CommandDigest = %s, want %s (golden vector drift)", v.Name, got, v.ExpectSHA)
		}
		if v.EquivalenceGroup != "" {
			if existing, ok := groupDigests[v.EquivalenceGroup]; ok {
				// 2. Same group -> same digest.
				if existing != got {
					t.Fatalf("equivalence group %q: vector %q produced %s but earlier vector produced %s — the group must share ONE digest (normalization is broken)", v.EquivalenceGroup, v.Name, got, existing)
				}
			} else {
				groupDigests[v.EquivalenceGroup] = got
			}
		}
	}

	// 2b. Different groups -> different digests.
	seenDigests := map[string]string{}
	for group, digest := range groupDigests {
		if otherGroup, ok := seenDigests[digest]; ok && otherGroup != group {
			t.Fatalf("equivalence groups %q and %q both produce %s — different groups must produce different digests", group, otherGroup, digest)
		}
		seenDigests[digest] = group
	}

	t.Logf("verified %d vectors across %d equivalence groups", len(doc.Vectors), len(groupDigests))
}
