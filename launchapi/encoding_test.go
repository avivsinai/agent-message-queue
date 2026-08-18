package launchapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResultV1GoldenEncoding(t *testing.T) {
	for _, test := range []struct {
		name   string
		file   string
		result any
	}{
		{name: "prepare", file: "prepare_result_v1.golden.json", result: goldenPrepareResultV1()},
		{name: "apply", file: "apply_result_v1.golden.json", result: goldenApplyResultV1()},
	} {
		t.Run(test.name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := MarshalResultV1(test.result)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != string(golden) {
				t.Fatalf("result encoding differs from %s\ngot:\n%s\nwant:\n%s", test.file, encoded, golden)
			}
		})
	}
	if _, err := MarshalResultV1(struct{}{}); err == nil {
		t.Fatal("canonical result encoder accepted a non-contract type")
	}
}

func TestResultV1GoldensRequireDigestAndAliasFields(t *testing.T) {
	for _, file := range []string{"prepare_result_v1.golden.json", "apply_result_v1.golden.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"subject_digest", "plan_digest", "trust_digest"} {
			if len(document[field]) == 0 {
				t.Fatalf("%s omits %s", file, field)
			}
		}
		if strings.HasPrefix(file, "apply") && string(document["semantic_digest"]) != string(document["trust_digest"]) {
			t.Fatalf("%s semantic_digest is not byte-equal to trust_digest", file)
		}
	}
}

func TestV061ResultGoldensRemainDecodeCompatible(t *testing.T) {
	for _, test := range []struct {
		file   string
		result any
	}{
		{file: "prepare_result_v0610.golden.json", result: &PrepareResultV1{}},
		{file: "apply_result_v0610.golden.json", result: &ApplyResultV1{}},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", test.file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, test.result); err != nil {
			t.Fatalf("decode v0.61.0 fixture %s: %v", test.file, err)
		}
		switch result := test.result.(type) {
		case *PrepareResultV1:
			if result.SubjectSchema != 0 || result.Preview.Capabilities != nil {
				t.Fatalf("v0.61.0 Prepare defaults changed: %#v", result)
			}
		case *ApplyResultV1:
			if result.SubjectSchema != 0 || result.Hints != nil {
				t.Fatalf("v0.61.0 Apply defaults changed: %#v", result)
			}
		}
	}
}

func goldenPrepareResultV1() PrepareResultV1 {
	return PrepareResultV1{
		ResultVersion: 1, SubjectSchema: SubjectSchemaV2, Outcome: PrepareOutcomeActionRequired, Reason: "untrusted_config_digest",
		SubjectDigest: goldenDigest('a'), PlanDigest: goldenDigest('b'), TrustDigest: goldenDigest('c'),
		PlannedWrites: []PlannedWriteV1{},
		RequiredActions: []RequiredActionV1{{
			ActionID: "trust-example", Kind: RequiredActionTrustConfirmation, Handles: []string{"claude"},
			Resources: []string{goldenDigest('c')}, AllowedDecisions: []DecisionChoiceV1{DecisionTrustExactSubject, DecisionDeny},
			ReasonCode: "untrusted_config_digest",
		}},
		Preview: PreviewV1{
			Target:  TargetV1{ProjectRoot: "/workspace/example", SessionRoot: "/workspace/example/.agent-mail/collab", Session: "collab"},
			Backend: "commands", Profile: "commands/portable/v1",
			Participants: []ParticipantPreviewV1{{Handle: "operator", Runnable: false, PlannedOutcome: "observe_only"}},
			Roster:       RosterDriftV1{Desired: []string{"operator"}, Present: []string{}, Missing: []string{"operator"}, Extra: []string{}},
			Capabilities: []ProviderCapabilitiesV1{},
		},
		Observations: []ParticipantObservationV1{{Handle: "operator", Mailbox: "missing", Runnable: false, Conversation: "none", Execution: "none", Resource: ""}},
	}
}

func goldenApplyResultV1() ApplyResultV1 {
	return ApplyResultV1{
		ResultVersion: 1, SubjectSchema: SubjectSchemaV2, Outcome: "applied", SubjectDigest: goldenDigest('a'), PlanDigest: goldenDigest('b'),
		TrustDigest: goldenDigest('c'), SemanticDigest: goldenDigest('c'), Backend: "commands", Profile: "commands/portable/v1",
		Roster: RosterDriftV1{Desired: []string{"claude"}, Present: []string{"claude"}, Missing: []string{}, Extra: []string{}},
		Observations: []ParticipantObservationV1{{
			Handle: "claude", Mailbox: "present", Runnable: true, Conversation: "ready", Execution: "acknowledged", Resource: "commands:claude",
		}},
		Commands: []CommandV1{{Argv: []string{"amq", "coop", "exec", "claude"}, Cwd: "/workspace/example"}},
	}
}

func goldenDigest(character byte) string { return "sha256:" + strings.Repeat(string(character), 64) }
