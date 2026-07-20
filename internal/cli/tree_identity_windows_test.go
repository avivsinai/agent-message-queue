//go:build windows

package cli

import "testing"

func TestInvalidWindowsIdentityRejectsSentinels(t *testing.T) {
	var zero, ff [16]byte
	for i := range ff {
		ff[i] = 0xff
	}
	if !invalidWindowsIdentity(1, zero) || !invalidWindowsIdentity(1, ff) {
		t.Fatal("zero and all-ff file IDs must be rejected")
	}
	var good [16]byte
	good[0] = 1
	if invalidWindowsIdentity(1, good) || invalidWindowsIdentity(0, good) || invalidWindowsIdentity(^uint64(0), good) {
		t.Fatal("valid file ID or sentinel volume was classified incorrectly")
	}
}

func TestValidWindowsIdentityTokenRejectsSentinels(t *testing.T) {
	if validPlatformTreeIdentityToken("v1:windows:0:01000000000000000000000000000000") {
		t.Fatal("zero volume accepted")
	}
	if validPlatformTreeIdentityToken("v1:windows:1:00000000000000000000000000000000") {
		t.Fatal("zero file ID accepted")
	}
}
