package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestApplyWrapperComposesArgvAndShiftsOwnedSlots(t *testing.T) {
	wrapper := testWrapper(t)
	input := "bootstrap"
	sum := sha256.Sum256([]byte(input))
	plan := AgentPlan{
		Handle: "claude", Argv: []string{"/usr/local/bin/claude", "--session-id", "00000000-0000-4000-8000-000000000000", input},
		Cwd: "/work", AdapterMode: AdapterModeMint, ResumePolicy: ResumeEnabled,
		LaunchNonce:  "00000000-0000-4000-8000-000000000000",
		DynamicArgv:  []DynamicArg{{Index: 2, Kind: DynamicArgLaunchNonce}},
		InitialInput: &PlannedInitialInput{Kind: InitialInputArgument, SHA256: "sha256:" + hex.EncodeToString(sum[:]), ArgvIndex: 3},
	}

	wrapped, err := applyWrapper(plan, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{wrapper.Executable, "--profile", "lead", "/usr/local/bin/claude", "--session-id", plan.LaunchNonce, input}
	if !slices.Equal(wrapped.Argv, want) {
		t.Fatalf("wrapped argv = %#v, want %#v", wrapped.Argv, want)
	}
	if wrapped.DynamicArgv[0].Index != 5 || wrapped.InitialInput.ArgvIndex != 6 {
		t.Fatalf("shifted slots = dynamic:%#v initial:%#v", wrapped.DynamicArgv, wrapped.InitialInput)
	}
	provider, err := providerExecutable(wrapped)
	if err != nil || provider != "/usr/local/bin/claude" {
		t.Fatalf("provider executable = %q, %v", provider, err)
	}
}

func TestWrapperProjectContainmentRejectsProjectPathAndAcceptsOutside(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(project, "wrapper")
	if err := os.WriteFile(inside, []byte("wrapper"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := validateWrapperFileForProject(&Wrapper{Executable: inside}, project)
	var pathErr *LaunchPathError
	if !errors.As(err, &pathErr) || pathErr.Code != WrapperProjectContainedCode {
		t.Fatalf("project wrapper error = %v, want %s", err, WrapperProjectContainedCode)
	}
	projectLink := filepath.Join(root, "project-link")
	if err := os.Symlink(inside, projectLink); err != nil {
		t.Fatal(err)
	}
	err = validateWrapperFileForProject(&Wrapper{Executable: projectLink}, project)
	if !errors.As(err, &pathErr) || pathErr.Code != WrapperProjectContainedCode {
		t.Fatalf("symlinked project wrapper error = %v, want %s", err, WrapperProjectContainedCode)
	}

	outside := filepath.Join(root, "wrapper")
	if err := os.WriteFile(outside, []byte("wrapper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateWrapperFileForProject(&Wrapper{Executable: outside}, project); err != nil {
		t.Fatalf("outside wrapper rejected: %v", err)
	}
}

func TestWrapperChangesPlanAndTrustDigests(t *testing.T) {
	wrapper := testWrapper(t)
	base := validPlan()
	wrapped := validPlan()
	var err error
	wrapped.Agents[0], err = applyWrapper(wrapped.Agents[0], wrapper)
	if err != nil {
		t.Fatal(err)
	}
	basePlan, _ := base.SemanticDigest()
	baseTrust, _ := base.TrustSemanticDigest()
	wrappedPlan, err := wrapped.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	wrappedTrust, err := wrapped.TrustSemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if basePlan == wrappedPlan || baseTrust == wrappedTrust {
		t.Fatalf("wrapper did not change both digests: plan=%t trust=%t", basePlan != wrappedPlan, baseTrust != wrappedTrust)
	}
	changed := wrapped
	changed.Agents = slices.Clone(wrapped.Agents)
	changed.Agents[0].Wrapper = cloneWrapper(wrapped.Agents[0].Wrapper)
	changed.Agents[0].Argv = slices.Clone(wrapped.Agents[0].Argv)
	changed.Agents[0].Wrapper.Args[1] = "reviewer"
	changed.Agents[0].Argv[2] = "reviewer"
	changedTrust, err := changed.TrustSemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changedTrust == wrappedTrust {
		t.Fatal("wrapper args did not change trust digest")
	}
}

func testWrapper(t *testing.T) *Wrapper {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seat-wrapper")
	if err := os.WriteFile(path, []byte("wrapper"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Wrapper{Executable: path, Args: []string{"--profile", "lead"}}
}
