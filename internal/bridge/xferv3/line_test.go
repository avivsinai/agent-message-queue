package xferv3

import "testing"

func TestLineFingerprintStableAndRequired(t *testing.T) {
	host, err := ParseHost("box")
	if err != nil {
		t.Fatal(err)
	}
	var x [32]byte
	x[0] = 7
	line, err := FormatLine(host, "host=box generation=1 public=aa", "ticket-1", x)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host.String() != "box" || got.Ticket != "ticket-1" || got.X25519 != x {
		t.Fatalf("line = %+v", got)
	}
	fp := got.Fingerprint()
	if len(fp) != FingerprintLen {
		t.Fatalf("fingerprint len = %d, want %d", len(fp), FingerprintLen)
	}
	again, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint() != fp {
		t.Fatal("fingerprint drifted")
	}
}
