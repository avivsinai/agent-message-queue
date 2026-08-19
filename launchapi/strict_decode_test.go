package launchapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStrictJSONRejectsDeepDocumentWithTypedCodeQuickly(t *testing.T) {
	const documentSize = 2 << 20
	depth := documentSize / 2
	raw := []byte(strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth))

	start := time.Now()
	var decoded any
	err := decodeStrictJSON(raw, &decoded)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("deep document took %s to reject; want bounded scan", elapsed)
	}
	var strictErr *StrictJSONError
	if !errors.As(err, &strictErr) || strictErr.Code != StrictJSONDepthExceeded {
		t.Fatalf("deep document error = %v, typed=%#v; want code %q", err, strictErr, StrictJSONDepthExceeded)
	}
}

func TestStrictJSONAcceptsSixtyFourNestedArrays(t *testing.T) {
	raw := []byte(strings.Repeat("[", 64) + "null" + strings.Repeat("]", 64))
	var decoded any
	if err := decodeStrictJSON(raw, &decoded); err != nil {
		t.Fatalf("64-depth document rejected: %v", err)
	}
}

func TestStrictJSONDepthBoundary(t *testing.T) {
	for _, test := range []struct {
		depth int
		want  StrictJSONErrorCode
	}{
		{depth: StrictJSONMaxDepth},
		{depth: StrictJSONMaxDepth + 1, want: StrictJSONDepthExceeded},
	} {
		t.Run(fmt.Sprintf("depth_%d", test.depth), func(t *testing.T) {
			raw := []byte(strings.Repeat("[", test.depth) + "null" + strings.Repeat("]", test.depth))
			var decoded any
			err := decodeStrictJSON(raw, &decoded)
			if test.want == "" {
				if err != nil {
					t.Fatalf("%d-depth document rejected: %v", test.depth, err)
				}
				return
			}
			var strictErr *StrictJSONError
			if !errors.As(err, &strictErr) || strictErr.Code != test.want {
				t.Fatalf("%d-depth document error = %v, typed=%#v; want code %q", test.depth, err, strictErr, test.want)
			}
		})
	}
}

func TestStrictJSONAcceptsCaseAndWhitespaceDistinctKeys(t *testing.T) {
	var decoded map[string]int
	if err := decodeStrictJSON([]byte(`{"Key":1," key ":2}`), &decoded); err != nil {
		t.Fatalf("case/whitespace-distinct keys rejected: %v", err)
	}
	if decoded["Key"] != 1 || decoded[" key "] != 2 {
		t.Fatalf("decoded keys = %#v", decoded)
	}
}

func TestStrictJSONDuplicateKeyReturnsTypedCode(t *testing.T) {
	var decoded map[string]int
	err := decodeStrictJSON([]byte(`{"outer":{"Key":1,"Key":2}}`), &decoded)
	var strictErr *StrictJSONError
	if !errors.As(err, &strictErr) || strictErr.Code != StrictJSONDuplicateKey {
		t.Fatalf("duplicate error = %v, typed=%#v; want code %q", err, strictErr, StrictJSONDuplicateKey)
	}
	if strictErr.Path != "$.outer" || strictErr.Key != "Key" {
		t.Fatalf("duplicate location = %#v; want $.outer/Key", strictErr)
	}
}

func FuzzDecodeStrictJSONRejectsDuplicateKeys(f *testing.F) {
	f.Add("fuzz-key")
	f.Add("nested/key")
	f.Fuzz(func(t *testing.T, key string) {
		encoded, err := json.Marshal(key)
		if err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"outer":[{"` + string(encoded[1:len(encoded)-1]) + `":1,"` + string(encoded[1:len(encoded)-1]) + `":2}]}`)
		var decoded map[string]any
		err = decodeStrictJSON(raw, &decoded)
		var strictErr *StrictJSONError
		if !errors.As(err, &strictErr) || strictErr.Code != StrictJSONDuplicateKey {
			t.Fatalf("key %q error = %v, typed=%#v", key, err, strictErr)
		}
	})
}
