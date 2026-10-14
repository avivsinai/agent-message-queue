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
