package xferv3

import (
	"errors"
	"testing"
)

func TestStorePeerRoundTrip(t *testing.T) {
	root := t.TempDir()
	host, err := ParseHost("box")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParseHandle("grok")
	if err != nil {
		t.Fatal(err)
	}
	var xPublic [32]byte
	xPublic[0] = 0xab
	peer, err := NewPeer(host, []Handle{allow}, "ticket", xPublic)
	if err != nil {
		t.Fatal(err)
	}
	if err := StorePeer(root, peer); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPeer(root, host)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host().String() != "box" || got.Ticket() != "ticket" || got.X25519Public() != xPublic {
		t.Fatalf("peer = %+v", got)
	}
	if len(got.AllowIn()) != 1 || got.AllowIn()[0].String() != "grok" {
		t.Fatalf("allow_in = %v", got.AllowIn())
	}
	if _, err := LoadPeer(root, mustHost(t, "mac")); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("missing peer = %v, want ErrNoPeer", err)
	}
}

func mustHost(t *testing.T, name string) Host {
	t.Helper()
	host, err := ParseHost(name)
	if err != nil {
		t.Fatal(err)
	}
	return host
}
