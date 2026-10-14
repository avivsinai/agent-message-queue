package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestP2PTwoPeersExchangeOverPipe(t *testing.T) {
	mac := newPeerHost(t, "mac", "codex", bytes.Repeat([]byte{0x41}, 32))
	box := newPeerHost(t, "box", "grok", bytes.Repeat([]byte{0x42}, 32))
	pinPeers(t, &mac, &box)
	pinPeers(t, &box, &mac)

	macToBox := writeOwed(t, mac, box, "hello-box", "hello from mac")
	boxToMac := writeOwed(t, box, mac, "hello-mac", "hello from box")
	log.Printf("p2p e2e: mac queued %s for box", macToBox)
	log.Printf("p2p e2e: box queued %s for mac", boxToMac)

	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	if err := SyncPipe(left, right, mac.state, box.state); err != nil {
		t.Fatalf("sync pipe: %v", err)
	}

	gotBox := readInbox(t, box.root, "grok")
	gotMac := readInbox(t, mac.root, "codex")
	if len(gotBox) != 1 || !bytes.Contains(gotBox[0], []byte("hello from mac")) {
		t.Fatalf("box inbox = %q", gotBox)
	}
	if len(gotMac) != 1 || !bytes.Contains(gotMac[0], []byte("hello from box")) {
		t.Fatalf("mac inbox = %q", gotMac)
	}
	if _, err := os.Stat(filepath.Join(mac.root, "bridge", "tx", "box", macToBox)); !os.IsNotExist(err) {
		t.Fatalf("mac outbox still holds %s: %v", macToBox, err)
	}
	if _, err := os.Stat(filepath.Join(box.root, "bridge", "tx", "mac", boxToMac)); !os.IsNotExist(err) {
		t.Fatalf("box outbox still holds %s: %v", boxToMac, err)
	}
	log.Printf("p2p e2e: box inbox has %d message(s), mac inbox has %d message(s)", len(gotBox), len(gotMac))
	log.Printf("p2p e2e: both receipts consumed the outbox files")
}

func TestP2POneSidedResponderHasMail(t *testing.T) {
	mac := newPeerHost(t, "mac", "codex", bytes.Repeat([]byte{0x61}, 32))
	box := newPeerHost(t, "box", "grok", bytes.Repeat([]byte{0x62}, 32))
	pinPeers(t, &mac, &box)
	pinPeers(t, &box, &mac)
	idText := writeOwed(t, box, mac, "one-sided", "only box sent this")

	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	done := make(chan error, 1)
	go func() {
		done <- SyncPipe(left, right, mac.state, box.state)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("one-sided sync: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("one-sided sync hung after the empty initiator sent DONE")
	}
	got := readInbox(t, mac.root, "codex")
	if len(got) != 1 || !bytes.Contains(got[0], []byte("only box sent this")) {
		t.Fatalf("mac inbox = %q", got)
	}
	if _, err := os.Stat(filepath.Join(box.root, "bridge", "tx", "mac", idText)); !os.IsNotExist(err) {
		t.Fatalf("box outbox still holds %s: %v", idText, err)
	}
}

type peerHost struct {
	root  string
	host  Host
	agent Handle
	key   ed25519.PrivateKey
	state HostState
}

func newPeerHost(t *testing.T, hostName, agent string, seed []byte) peerHost {
	t.Helper()
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	if err := bridge.WriteHostID(root, hostName); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(seed)
	if err := bridge.WriteIdentity(root, bridge.HostKey{Generation: "1", Private: key}); err != nil {
		t.Fatal(err)
	}
	static, err := StaticFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseHost(hostName)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := ParseHandle(agent)
	if err != nil {
		t.Fatal(err)
	}
	return peerHost{
		root:  root,
		host:  h,
		agent: handle,
		key:   key,
		state: HostState{Root: root, Local: h, LocalKey: key, Static: static},
	}
}

func pinPeers(t *testing.T, local, remote *peerHost) {
	t.Helper()
	peer, err := NewPeer(remote.host, []Handle{remote.agent}, "", remote.state.Static.Public())
	if err != nil {
		t.Fatal(err)
	}
	if err := StorePeer(local.root, peer); err != nil {
		t.Fatal(err)
	}
	if err := bridge.WriteTrusted(local.root, remote.host.String(), remote.key.Public().(ed25519.PublicKey), "1"); err != nil {
		t.Fatal(err)
	}
	local.state.Snap = Snapshot{
		Local:   local.host,
		Peer:    remote.host,
		PeerKey: remote.key.Public().(ed25519.PublicKey),
		X25519:  remote.state.Static.Public(),
		AllowIn: []Handle{local.agent},
	}
}

func writeOwed(t *testing.T, from, to peerHost, id, body string) string {
	t.Helper()
	payload, err := (format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    from.agent.String(),
			To:      []string{to.agent.String()},
			Thread:  "p2p/" + from.agent.String() + "__" + to.agent.String(),
			Created: time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		},
		Body: body,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dest, err := ParseDest(to.host.String() + "/" + to.agent.String())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecord(from.host, from.agent, dest, mustMessage(t, id), mustThread(t, "p2p/"+from.agent.String()+"__"+to.agent.String()), payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := PutTx(from.root, from.key, rec, payload)
	if err != nil {
		t.Fatal(err)
	}
	return got.String()
}

func readInbox(t *testing.T, root, agent string) [][]byte {
	t.Helper()
	dir := filepath.Join(root, "agents", agent, "inbox", "new")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	return out
}
