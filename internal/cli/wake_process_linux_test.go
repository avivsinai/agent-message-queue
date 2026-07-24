//go:build linux

package cli

import (
	"strings"
	"testing"
)

func TestLinuxProcStartTokenHandlesParensInCommand(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[19] = "123456"
	stat := "42 (amq wake ) with spaces) " + strings.Join(fields, " ")

	token, err := linuxProcStartToken(stat)
	if err != nil {
		t.Fatalf("linuxProcStartToken: %v", err)
	}
	if token != "123456" {
		t.Fatalf("token = %q, want 123456", token)
	}
}

func TestLinuxProcStartTokenRejectsMalformedStat(t *testing.T) {
	if _, err := linuxProcStartToken("42 amq S 0 0"); err == nil {
		t.Fatal("expected malformed stat error")
	}
}

func TestLinuxProcIdentityRecognizesZombie(t *testing.T) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "Z"
	fields[19] = "123456"
	token, state, err := linuxProcIdentity("42 (amq wake) " + strings.Join(fields, " "))
	if err != nil {
		t.Fatalf("linuxProcIdentity: %v", err)
	}
	if token != "123456" || state != "Z" {
		t.Fatalf("identity = token %q state %q, want 123456/Z", token, state)
	}
}

func TestSplitProcCmdline(t *testing.T) {
	got := splitProcCmdline([]byte("/bin/amq\x00wake\x00--me\x00codex\x00"))
	want := []string{"/bin/amq", "wake", "--me", "codex"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
