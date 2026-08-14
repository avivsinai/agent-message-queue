package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestExecutionTrustDigestBindsSessionAndPhysicalRoot(t *testing.T) {
	_, firstRoot := openTestRoot(t)
	_, secondRoot := openTestRoot(t)
	plan := validPlan()
	first, err := ExecutionTrustDigest(plan, "collab", firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := ExecutionTrustDigest(plan, "empty", firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	otherRoot, err := ExecutionTrustDigest(plan, "collab", secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first == otherSession {
		t.Fatal("session change retained stale execution trust digest")
	}
	if first == otherRoot {
		t.Fatal("physical session-root change retained stale execution trust digest")
	}
}

func TestPrepareTrustDigestMatchesExistingTrustSubjectForPresentRoot(t *testing.T) {
	_, root := openTestRoot(t)
	plan := validPlan()
	planDigest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity, err := fsq.StableTreeIdentityInfo(root.FileInfo())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareTrustDigest(planDigest, "collab", root.Base(), rootIdentity)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := ExecutionTrustDigest(plan, "collab", root)
	if err != nil {
		t.Fatal(err)
	}
	if prepared != existing {
		t.Fatalf("Prepare trust digest %q differs from existing execution trust digest %q", prepared, existing)
	}
}

func TestExecutionTrustDigestRejectsSamePathRootReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "session")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	open := func() *fsq.DeliveryRoot {
		identity, err := fsq.SnapshotDeliveryRoot(path)
		if err != nil {
			t.Fatal(err)
		}
		root, err := fsq.OpenDeliveryRoot(path, identity)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	firstRoot := open()
	first, err := ExecutionTrustDigest(validPlan(), "collab", firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(parent, "detached")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	secondRoot := open()
	defer func() { _ = secondRoot.Close() }()
	second, err := ExecutionTrustDigest(validPlan(), "collab", secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("same-path session-root replacement retained stale execution trust digest")
	}
}
