//go:build !unix

package main

import "testing"

// bbn r6: an unsupported platform is quiet on purpose. With the REAL
// platform implementations (no list, unsupported identity), noteStaleACP
// must add zero notes — a healthy Windows doctor must not grow a
// permanent reinstall warning that no reinstall can clear.
func TestNoteStaleACPIsQuietOnUnsupportedPlatforms(t *testing.T) {
	var notes []string
	note := func(boundary, subject, detail, remedy string) {
		notes = append(notes, boundary+"/"+subject+": "+detail)
	}
	noteStaleACP(note)
	if len(notes) != 0 {
		t.Fatalf("unsupported platform must add zero notes, got %v", notes)
	}
	if staleACPSupported {
		t.Fatal("staleACPSupported must be false on unsupported platforms")
	}
}
