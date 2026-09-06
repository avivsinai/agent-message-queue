package launch

import (
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"testing"
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
	prepared, err := PrepareTrustDigest(planDigest, "collab", root.Base(), rootIdentity, nil)
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
