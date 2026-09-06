package launchapi

import (
	"errors"
	"strings"
	"testing"
)

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

// Regression for issue #595: an unbounded-depth document OOM-killed the
// process. The structural scan must fail closed with a typed code at the
// first depth over the limit.
func TestStrictJSONRejectsDepthOverLimitWithTypedCode(t *testing.T) {
	raw := []byte(strings.Repeat("[", StrictJSONMaxDepth+1) + "null" + strings.Repeat("]", StrictJSONMaxDepth+1))
	var decoded any
	err := decodeStrictJSON(raw, &decoded)
	var strictErr *StrictJSONError
	if !errors.As(err, &strictErr) || strictErr.Code != StrictJSONDepthExceeded {
		t.Fatalf("depth %d error = %v, typed=%#v; want code %q", StrictJSONMaxDepth+1, err, strictErr, StrictJSONDepthExceeded)
	}
	var decodedOK any
	if err := decodeStrictJSON([]byte(strings.Repeat("[", StrictJSONMaxDepth)+"null"+strings.Repeat("]", StrictJSONMaxDepth)), &decodedOK); err != nil {
		t.Fatalf("depth %d at the limit rejected: %v", StrictJSONMaxDepth, err)
	}
}
