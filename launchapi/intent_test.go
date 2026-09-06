package launchapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func validIntentJSON(t *testing.T) string {
	t.Helper()
	return `{
  "intent_version": 1,
  "participants": [
    {"handle":"operator","runnable":false},
    {
      "handle":"codex",
      "runnable":true,
      "executable":"codex",
      "args":["--dangerously-bypass-approvals-and-sandbox"],
      "cwd":{"kind":"absolute","path":` + quotedJSON(t, filepath.Clean(t.TempDir())) + `},
      "env_overlay":{"LANG":"C"},
      "resume_policy":"resume",
      "execution":{
        "require_wake":true,
        "no_gitignore":true,
        "wake":{"mode":"enabled","injector":{"mode":"raw","via":"/opt/amq/inject","args":["send"]}},
        "integrations":{"symphony":{"events":["after_create","before_run","after_run","before_remove"],"workspace_key":"team-17"}}
      }
    }
  ]
}`
}

func quotedJSON(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLaunchIntentRejectsAMQOwnedFields(t *testing.T) {
	base := `{"intent_version":1,"participants":[{"handle":"codex","runnable":true,"executable":"codex","cwd":{"kind":"relative","path":"."},"resume_policy":"fresh","execution":{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled"}}}]}`
	for _, field := range []string{
		"adapter_mode",
		"launch_nonce",
		"conversation_id",
		"dynamic_argv",
		"unknown",
	} {
		t.Run(field, func(t *testing.T) {
			hostile := strings.Replace(base, `"runnable":true`, `"runnable":true,"`+field+`":"forged"`, 1)
			if _, err := DecodeLaunchIntentV1([]byte(hostile)); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("DecodeLaunchIntentV1(%s) error = %v, want unknown-field refusal", field, err)
			}
		})
	}
}

func TestLaunchIntentV1AcceptsSiblingWorktreeAndZeroRunnable(t *testing.T) {
	intent, err := DecodeLaunchIntentV1([]byte(validIntentJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(intent.Participants) != 2 || intent.Participants[0].Runnable || !intent.Participants[1].Runnable {
		t.Fatalf("participants = %#v", intent.Participants)
	}

	participantOnly, err := DecodeLaunchIntentV1([]byte(`{"intent_version":1,"participants":[{"handle":"operator","runnable":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if participantOnly.Participants[0].Runnable {
		t.Fatal("participant-only intent became runnable")
	}
}

func TestLaunchIntentV1UsesAdapterDenyByDefaultGrammar(t *testing.T) {
	base := `{"intent_version":1,"participants":[{"handle":"codex","runnable":true,"executable":%s,"args":%s,"env_overlay":%s,"cwd":{"kind":"relative","path":"."},"resume_policy":"fresh","execution":{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled"}}}]}`
	tests := []struct {
		name       string
		executable string
		args       string
		env        string
		want       string
	}{
		{name: "unknown provider", executable: "bash", args: `[]`, env: `{}`, want: "adapter-known provider"},
		{name: "unknown flag", executable: "codex", args: `["--config","pwned=true"]`, env: `{}`, want: "not allowed"},
		{name: "unknown env", executable: "codex", args: `[]`, env: `{"CODEX_HOME":"/tmp/attacker"}`, want: "not allowed"},
		{name: "forged model value", executable: "codex", args: `["--model","--sandbox"]`, env: `{}`, want: "invalid value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := fmt.Sprintf(base, quotedJSON(t, test.executable), test.args, test.env)
			if _, err := DecodeLaunchIntentV1([]byte(raw)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeLaunchIntentV1 error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLaunchIntentV1MarshalRoundTrip(t *testing.T) {
	intent, err := DecodeLaunchIntentV1([]byte(validIntentJSON(t)))
	if err != nil {
		t.Fatal(err)
	}
	intent.Participants[1].Wrapper = &WrapperV1{
		Executable: filepath.Join(t.TempDir(), "seat-wrapper"),
		Args:       []string{"--profile", "lead"},
	}
	raw, err := MarshalLaunchIntentV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLaunchIntentV1(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Participants) != len(intent.Participants) {
		t.Fatalf("round-trip participant count = %d, want %d", len(decoded.Participants), len(intent.Participants))
	}
	wrapper := decoded.Participants[1].Wrapper
	if wrapper == nil || wrapper.Executable != intent.Participants[1].Wrapper.Executable ||
		len(wrapper.Args) != 2 || wrapper.Args[0] != "--profile" || wrapper.Args[1] != "lead" {
		t.Fatalf("round-trip wrapper = %#v", wrapper)
	}
}

func TestInitialInputValidationReturnsTypedCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		code InitialInputErrorCode
	}{
		{name: "nul", text: "before\x00after", code: InitialInputControl},
		{name: "delete", text: "before\x7fafter", code: InitialInputControl},
		{name: "leading dash", text: "-danger", code: InitialInputLeadingDash},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := LaunchIntentV1{
				IntentVersion: IntentVersionV1,
				Participants: []ParticipantV1{{
					Handle: "codex", Runnable: true, Executable: "codex",
					Cwd:          &WorkingDirectoryV1{Kind: WorkingDirectoryAbsolute, Path: t.TempDir()},
					ResumePolicy: ResumePolicyFresh,
					Execution:    &ExecutionOptionsV1{Wake: WakeOptionsV1{Mode: WakeEnabled}},
					InitialInput: &InitialInputV1{Kind: InitialInputArgument, Text: test.text},
				}},
			}
			var typed *InitialInputValidationError
			if err := intent.Validate(); !errors.As(err, &typed) || typed.Code != test.code {
				t.Fatalf("Validate error = %v, typed=%#v; want code %q", err, typed, test.code)
			}
		})
	}
}

func TestStrictJSONErrorMessageCarriesCodeAndPath(t *testing.T) {
	dup := &StrictJSONError{Code: StrictJSONDuplicateKey, Path: "$.outer", Key: "Key"}
	if got := dup.Error(); got == "" || !strings.Contains(got, "duplicate") || !strings.Contains(got, "$.outer") {
		t.Fatalf("duplicate StrictJSONError message = %q, want code and path", got)
	}
	depth := &StrictJSONError{Code: StrictJSONDepthExceeded, Path: "$.deep"}
	if got := depth.Error(); !strings.Contains(got, "maximum depth") {
		t.Fatalf("depth StrictJSONError message = %q, want depth guidance", got)
	}
}
