package ack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAckMarshal(t *testing.T) {
	ts := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	a := New("msg-1", "p2p/codex__cloudcode", "cloudcode", "codex", ts)
	data, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected trailing newline")
	}
	var out Ack
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.MsgID != "msg-1" || out.Thread == "" || out.From != "cloudcode" || out.To != "codex" {
		t.Fatalf("unexpected ack: %+v", out)
	}
}
