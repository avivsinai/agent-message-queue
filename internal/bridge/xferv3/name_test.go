package xferv3

import (
	"errors"
	"testing"
)

func TestParseHostAllowsDotAndRefusesDotNames(t *testing.T) {
	host, err := ParseHost("a.b")
	if err != nil {
		t.Fatalf("ParseHost(a.b): %v", err)
	}
	if host.String() != "a.b" {
		t.Fatalf("ParseHost(a.b) = %q", host)
	}
	if _, err := ParseHost("."); !errors.Is(err, ErrName) {
		t.Fatalf("ParseHost(.) = %v, want ErrName", err)
	}
	if _, err := ParseHost(".."); !errors.Is(err, ErrName) {
		t.Fatalf("ParseHost(..) = %v, want ErrName", err)
	}
}

// v3 names follow the one bridge identifier grammar: a leading '_' is a
// valid AMQ handle, a leading '.' or '-' is refused (audit 2026-11-10).
func TestNamesFollowTheBridgeIdentifierGrammar(t *testing.T) {
	if _, err := ParseHandle("_bot"); err != nil {
		t.Fatalf("ParseHandle(_bot): %v", err)
	}
	for _, name := range []string{"-x", ".x"} {
		if _, err := ParseHandle(name); !errors.Is(err, ErrName) {
			t.Fatalf("ParseHandle(%s) = %v, want ErrName", name, err)
		}
		if _, err := ParseHost(name); !errors.Is(err, ErrName) {
			t.Fatalf("ParseHost(%s) = %v, want ErrName", name, err)
		}
	}
}
