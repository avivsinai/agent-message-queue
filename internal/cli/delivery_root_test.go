package cli

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func openDeliveryRootForCLITest(t testing.TB, base string) *fsq.DeliveryRoot {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot(%s): %v", base, err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot(%s): %v", base, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func deliverToInboxForTest(t testing.TB, base, agent, filename string, data []byte) (string, error) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		return "", err
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	return fsq.DeliverToInbox(root, agent, filename, data)
}

func deliverToInboxesForTest(t testing.TB, base string, recipients []string, filename string, data []byte) (map[string]string, error) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		return nil, err
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return fsq.DeliverToInboxes(root, recipients, filename, data)
}
