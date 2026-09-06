package cli

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func deliverInvalidDLQTransitionFixture(t *testing.T, root, agent, id string) {
	t.Helper()
	if err := deliverInvalidDLQTransitionFixtureError(root, agent, id); err != nil {
		t.Fatalf("deliver invalid fixture: %v", err)
	}
}

func deliverInvalidDLQTransitionFixtureError(root, agent, id string) error {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	_, err = fsq.DeliverToInbox(deliveryRoot, agent, id+".md", []byte("missing frontmatter"))
	return err
}
