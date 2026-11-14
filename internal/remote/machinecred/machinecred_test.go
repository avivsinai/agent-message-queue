package machinecred

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// The owner signs the request a minted agent prints; the enrolled grant
// loads back, verifies against the agent key, and holds inside its bound.
func TestMintRequestSignEnrollLoad(t *testing.T) {
	home := t.TempDir()
	pub, err := Mint(home)
	if err != nil {
		t.Fatal(err)
	}
	if agents, err := Agents(home); err != nil || len(agents) != 1 || agents[0] != pub {
		t.Fatalf("Agents = %v, %v; want [%s]", agents, err, pub)
	}
	notAfter := time.Now().Add(90 * 24 * time.Hour).Unix()
	name, body, conds, err := ParseRequest(Request(pub, "AMQ Remote @ host", notAfter))
	if err != nil || name != "AMQ Remote @ host" || body != pub || conds != bodykey.MachineConditions(notAfter) {
		t.Fatalf("ParseRequest = %q %q %q, %v", name, body, conds, err)
	}

	var owner [32]byte
	owner[31] = 7
	tag, err := bodykey.SignAuthTag(owner, body, conds)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := EncodeGrant(body, *tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(home, grant); err != nil {
		t.Fatal(err)
	}

	cred, err := Load(home, pub)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Key.PublicKeyHex() != pub || cred.Tag.OwnerPubKey != tag.OwnerPubKey {
		t.Fatalf("loaded %s / %s", cred.Key.PublicKeyHex(), cred.Tag.OwnerPubKey)
	}
	if err := cred.Tag.Verify(pub); err != nil {
		t.Fatal(err)
	}
	if err := cred.Tag.Window(time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
}
