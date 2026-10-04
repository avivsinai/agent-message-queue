package protocol

import (
	"strings"
	"testing"
)

// TestRefBoundFitsMaxSizeKey pins BK6: a request reference built from the
// maximum opaque-segment lengths (creator_host and target_id each MaxOpaqueLen,
// plus a fixed-length UUID) must match the ref regex and round-trip through
// DecodeRef. The previous 200-char cap rejected these valid max-size refs
// (up to 477 chars), so a receipt for a max-size key could not be reused.
func TestRefBoundFitsMaxSizeKey(t *testing.T) {
	host := strings.Repeat("a", MaxOpaqueLen)
	tgt := strings.Repeat("b", MaxOpaqueLen)
	id := "11111111-1111-4111-8111-111111111501"
	ref := EncodeRef(host, tgt, id)
	if uint(len(ref)) > uint(MaxRefLen) {
		t.Fatalf("max-size ref len %d exceeds MaxRefLen %d", len(ref), MaxRefLen)
	}
	if !refRe.MatchString(ref) {
		t.Fatalf("max-size ref (len %d) does not match refRe (cap %d)", len(ref), MaxRefLen)
	}
	gotHost, gotTgt, gotID, err := DecodeRef(ref)
	if err != nil {
		t.Fatalf("max-size ref did not decode: %v", err)
	}
	if gotHost != host || gotTgt != tgt || gotID != id {
		t.Fatalf("round-trip mismatch: %s %s %s", gotHost, gotTgt, gotID)
	}
}
