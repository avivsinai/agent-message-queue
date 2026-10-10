package jcs

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestVectors reproduces testdata/link/jcs/vectors.json, the vectors the
// browser, Python and Go canonicalizers share.
func TestVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "link", "jcs", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Vectors []struct {
			Name         string `json:"name"`
			Input        string `json:"input"`
			CanonicalHex string `json:"canonical_hex"`
			Refused      bool   `json:"refused"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range doc.Vectors {
		got, err := Canonicalize([]byte(v.Input))
		if v.Refused {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: want refused, got %q, %v", v.Name, got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		if hex.EncodeToString(got) != v.CanonicalHex {
			t.Errorf("%s: got %q, want hex %s", v.Name, got, v.CanonicalHex)
		}
	}
}
