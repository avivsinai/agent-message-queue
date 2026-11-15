package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
)

// `machine request` makes the agent key and prints its request; `machine
// enroll` stores the grant the owner signed for it and names the agent, the
// owner and the end date.
func TestMachineRequestThenEnroll(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", dir)

	var reqOut, reqErr bytes.Buffer
	if code := run([]string{"machine", "request", "--valid-for", "24h", "--name", "AMQ Remote @ box"}, nil, &reqOut, &reqErr); code != 0 {
		t.Fatalf("machine request exit %d: %s", code, reqErr.String())
	}
	t.Logf("machine request stdout:\n%sstderr:\n%s", reqOut.String(), reqErr.String())
	name, body, conds, err := machinecred.ParseRequest(reqOut.String())
	if err != nil || name != "AMQ Remote @ box" {
		t.Fatalf("request %q: %v", reqOut.String(), err)
	}

	var owner [32]byte
	owner[31] = 7
	tag, err := bodykey.SignAuthTag(owner, body, conds)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := machinecred.EncodeGrant(body, "wss://relay.example", *tag)
	if err != nil {
		t.Fatal(err)
	}
	grantPath := filepath.Join(dir, "grant.json")
	if err := os.WriteFile(grantPath, grant, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := run([]string{"machine", "enroll", "--grant", grantPath}, nil, &out, &errb); code != 0 {
		t.Fatalf("machine enroll exit %d: %s", code, errb.String())
	}
	t.Logf("machine enroll stdout:\n%s", out.String())
	end, _ := tag.NotAfter()
	for _, want := range []string{
		"agent pubkey: " + body,
		"owner pubkey: " + tag.OwnerPubKey,
		"relay: wss://relay.example",
		"valid until " + time.Unix(end, 0).UTC().Format(time.RFC3339),
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("enroll stdout %q lacks %q", out.String(), want)
		}
	}
}
