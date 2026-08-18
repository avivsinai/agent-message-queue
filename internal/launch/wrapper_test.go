package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestWrapperValidationAcceptsRegularFileAndResolvableSymlink(t *testing.T) {
	wrapper := testWrapper(t)
	if err := validateWrapperFile(wrapper); err != nil {
		t.Fatalf("regular wrapper: %v", err)
	}
	link := filepath.Join(t.TempDir(), "wrapper-link")
	if err := os.Symlink(wrapper.Executable, link); err != nil {
		t.Fatal(err)
	}
	if err := validateWrapperFile(&Wrapper{Executable: link}); err != nil {
		t.Fatalf("symlink wrapper: %v", err)
	}
	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "missing"), want: "stat executable"},
		{name: "directory", path: t.TempDir(), want: "regular file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateWrapperFile(&Wrapper{Executable: test.path}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateWrapperFile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWrapperValidationRejectsNonExecutableRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrapper")
	if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateWrapperFile(&Wrapper{Executable: path}); err == nil || !strings.Contains(err.Error(), "execute") {
		t.Fatalf("non-executable wrapper error = %v", err)
	}
}

func TestWrapperValidateRejectsUnsafeSyntax(t *testing.T) {
	for _, test := range []struct {
		name    string
		wrapper Wrapper
		want    string
	}{
		{name: "relative path", wrapper: Wrapper{Executable: "bin/seat-wrapper"}, want: "absolute path"},
		{name: "PATH lookup", wrapper: Wrapper{Executable: "seat-wrapper"}, want: "absolute path"},
		{name: "shell fragment", wrapper: Wrapper{Executable: "sh -c"}, want: "absolute path"},
		{name: "unclean path", wrapper: Wrapper{Executable: "/opt/bin/../seat-wrapper"}, want: "clean absolute"},
		{name: "executable NUL", wrapper: Wrapper{Executable: "/opt/seat\x00wrapper"}, want: "without NUL"},
		{name: "empty arg", wrapper: Wrapper{Executable: "/opt/seat-wrapper", Args: []string{""}}, want: "must not be empty"},
		{name: "arg NUL", wrapper: Wrapper{Executable: "/opt/seat-wrapper", Args: []string{"bad\x00arg"}}, want: "without NUL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.wrapper.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Wrapper.Validate error = %v, want %q", err, test.want)
			}
		})
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

func TestPrepareWrapperExecutableIdentityBindsV2SubjectOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seat-wrapper")
	writeWrapperFixture(t, path, "#!/bin/sh\necho one\n")
	fixture := newInternalPrepareFixture(t)
	fixture.request.Participants[0].Wrapper = &Wrapper{Executable: path, Args: []string{"--profile", "lead"}}

	first := prepareWrapperFixture(t, fixture, SubjectSchemaV2)
	if len(first.Participants) != 1 || first.Participants[0].Wrapper == nil {
		t.Fatalf("v2 subject omitted wrapper identity: %#v", first.Participants)
	}
	probed, err := ProbeExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := MarshalExecutableIdentity(probed)
	if err != nil {
		t.Fatal(err)
	}
	got := first.Participants[0].Wrapper
	if got.Requested != path || got.Consulted != path || !bytes.Equal(got.Identity, want) {
		t.Fatalf("wrapper identity = %#v, want path %q identity %s", got, path, want)
	}

	replacement := path + ".next"
	writeWrapperFixture(t, replacement, "#!/bin/sh\necho two\n")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	second := prepareWrapperFixture(t, fixture, SubjectSchemaV2)
	if first.SubjectDigest == second.SubjectDigest {
		t.Fatal("same-path wrapper replacement kept v2 subject digest")
	}
	if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest {
		t.Fatalf("wrapper replacement churned plan/trust first=%s/%s second=%s/%s",
			first.PlanDigest, first.TrustDigest, second.PlanDigest, second.TrustDigest)
	}

	v1First := prepareWrapperFixture(t, fixture, SubjectSchemaV1)
	writeWrapperFixture(t, replacement, "#!/bin/sh\necho tri\n")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	v1Second := prepareWrapperFixture(t, fixture, SubjectSchemaV1)
	if v1First.SubjectDigest != v1Second.SubjectDigest || v1First.PlanDigest != v1Second.PlanDigest || v1First.TrustDigest != v1Second.TrustDigest {
		t.Fatalf("v1 wrapper replacement changed digests first=%s/%s/%s second=%s/%s/%s",
			v1First.SubjectDigest, v1First.PlanDigest, v1First.TrustDigest,
			v1Second.SubjectDigest, v1Second.PlanDigest, v1Second.TrustDigest)
	}
	if v1First.Participants[0].Wrapper != nil {
		t.Fatalf("v1 subject included wrapper identity: %#v", v1First.Participants[0].Wrapper)
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

func writeWrapperFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func prepareWrapperFixture(t *testing.T, fixture internalPrepareFixture, schema int) PrepareResult {
	t.Helper()
	request := fixture.request
	request.SubjectSchema = schema
	result, err := Prepare(context.Background(), request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	return result
}
