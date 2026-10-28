package buzzio

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// Pre-merge review of bead 611.42.12.1 (P1): with this machine's clock 300 s
// ahead of the relay, a deletion by the agent stored after the read and dated
// 890 s before the relay's clock, which the relay accepts, fell before the
// window and was not seen. It must refuse as altered.
func TestDesktopHistorySeesABackdatedAgentDeletionUnderClockSkew(t *testing.T) {
	var owner, agent [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(agent[:])
	agentHex, ownerHex := nostr.GetPublicKey(agent).Hex(), nostr.GetPublicKey(owner).Hex()
	tag, err := bodykey.SignAuthTag(owner, agentHex, bodykey.ShareConditions(relay.KindAuth, time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	wire := []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
	fakeRelay, srv, url := relaytest.Start(agentHex, wire)
	defer srv.Close()
	ctx := context.Background()
	conn, err := relay.Connect(ctx, relay.Config{URL: url, Secret: agent, AuthTag: wire})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	relayNow := time.Now()
	localNow := relayNow.Add(300 * time.Second)
	before, err := ReadAgentDeletions(ctx, conn, agentHex, localNow)
	if err != nil {
		t.Fatal(err)
	}
	msg := nostr.Event{CreatedAt: nostr.Timestamp(localNow.Unix()), Kind: KindDM, Content: "x", Tags: nostr.Tags{{"h", "c"}}}
	if err := msg.Sign(agent); err != nil {
		t.Fatal(err)
	}
	fakeRelay.Inject(msg)
	deletion := nostr.Event{CreatedAt: nostr.Timestamp(relayNow.Add(-890 * time.Second).Unix()), Kind: KindDeletion, Tags: nostr.Tags{{"e", strings.Repeat("7", 64)}}}
	if err := deletion.Sign(agent); err != nil {
		t.Fatal(err)
	}
	fakeRelay.Inject(deletion)
	if err := CheckDesktopHistory(ctx, conn, msg, ownerHex, before); !errors.Is(err, ErrAltered) {
		t.Fatalf("CheckDesktopHistory = %v; want ErrAltered for a backdated deletion stored after the read", err)
	}
}

// Pre-merge review of bead 611.42.12.1 (P2): a clock that steps back between
// the read before the post and the post leaves the read short of the
// message's own date. Such a read must not cover the message.
func TestAgentDeletionsReadBeforeAClockStepDoNotCoverTheMessage(t *testing.T) {
	msg := nostr.Event{CreatedAt: nostr.Timestamp(time.Now().Unix())}
	stepped := AgentDeletions{Since: msg.CreatedAt + 1200 - 1800}
	if stepped.Covers(msg) {
		t.Fatal("a read taken 20 min ahead of the message's date covers it; want not covered")
	}
	if !(AgentDeletions{Since: msg.CreatedAt - 1800}).Covers(msg) {
		t.Fatal("a read taken at the message's date does not cover it")
	}
}
