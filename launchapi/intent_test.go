package launchapi

import (
	"encoding/json"
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

func TestLaunchIntentNonRunnableIsHandleOnly(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing runnable", raw: `{"intent_version":1,"participants":[{"handle":"operator"}]}`, want: "requires runnable"},
		{name: "omitted participants", raw: `{"intent_version":1}`, want: "requires participants"},
		{name: "extra executable", raw: `{"intent_version":1,"participants":[{"handle":"operator","runnable":false,"executable":"codex"}]}`, want: "exactly handle and runnable"},
		{name: "extra empty args", raw: `{"intent_version":1,"participants":[{"handle":"operator","runnable":false,"args":[]}]}`, want: "exactly handle and runnable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeLaunchIntentV1([]byte(test.raw)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeLaunchIntentV1 error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLaunchIntentOnLivePolicy(t *testing.T) {
	base := `{"intent_version":1,"participants":[{"handle":"codex","runnable":true,"executable":"codex","cwd":{"kind":"relative","path":"."},"resume_policy":"fresh","execution":{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled"}}%s}]}`
	if _, err := DecodeLaunchIntentV1([]byte(fmt.Sprintf(base, `,"on_live":"keep"`))); err != nil {
		t.Fatalf("keep rejected: %v", err)
	}
	if _, err := DecodeLaunchIntentV1([]byte(fmt.Sprintf(base, `,"on_live":"refuse"`))); err != nil {
		t.Fatalf("refuse rejected: %v", err)
	}
	if _, err := DecodeLaunchIntentV1([]byte(fmt.Sprintf(base, `,"on_live":"resume"`))); err == nil || !strings.Contains(err.Error(), "on_live") {
		t.Fatalf("invalid on_live error = %v", err)
	}
	hostile := `{"intent_version":1,"participants":[{"handle":"operator","runnable":false,"on_live":"keep"}]}`
	if _, err := DecodeLaunchIntentV1([]byte(hostile)); err == nil || !strings.Contains(err.Error(), "exactly handle and runnable") {
		t.Fatalf("non-runnable on_live error = %v", err)
	}
}

func TestNonRunnableValidateRejectsSmuggledGoFields(t *testing.T) {
	participant := ParticipantV1{Handle: "operator", Runnable: false, Executable: "codex"}
	if err := participant.validate(); err == nil || !strings.Contains(err.Error(), "handle-only") {
		t.Fatalf("validate error = %v, want handle-only refusal", err)
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

func TestLaunchIntentV1RejectsHostileTypedOptions(t *testing.T) {
	base := `{"intent_version":1,"participants":[{"handle":"codex","runnable":true,"executable":"codex","cwd":{"kind":"relative","path":"."},"resume_policy":"fresh","execution":%s}]}`
	tests := []struct {
		name      string
		execution string
		want      string
	}{
		{name: "disabled without reason", execution: `{"require_wake":false,"no_gitignore":false,"wake":{"mode":"disabled"}}`, want: "audit reason"},
		{name: "require disabled", execution: `{"require_wake":true,"no_gitignore":false,"wake":{"mode":"disabled","audit_reason":"operator policy"}}`, want: "conflicts"},
		{name: "relative injector", execution: `{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled","injector":{"mode":"raw","via":"bin/inject"}}}`, want: "absolute"},
		{name: "args without via", execution: `{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled","injector":{"mode":"raw","args":["send"]}}}`, want: "require via"},
		{name: "unknown symphony event", execution: `{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled"},"integrations":{"symphony":{"events":["after_create","shell_hook"]}}}`, want: "unknown symphony event"},
		{name: "arbitrary hook", execution: `{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled"},"hooks":{"before_run":"touch /tmp/pwned"}}`, want: "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeLaunchIntentV1([]byte(strings.Replace(base, "%s", test.execution, 1)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeLaunchIntentV1 error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLaunchIntentV1RejectsExplicitNullSmuggling(t *testing.T) {
	base := `{"intent_version":1,"participants":[{"handle":"codex","runnable":true,"executable":"codex","args":[],"cwd":{"kind":"relative","path":"."},"env_overlay":{},"resume_policy":"fresh","execution":{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled","injector":{"mode":"none"}},"integrations":{"symphony":{"events":["after_create"]}}}}]}`
	for _, replacement := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "args", old: `"args":[]`, new: `"args":null`},
		{name: "env", old: `"env_overlay":{}`, new: `"env_overlay":null`},
		{name: "injector", old: `"injector":{"mode":"none"}`, new: `"injector":null`},
		{name: "integrations", old: `"integrations":{"symphony":{"events":["after_create"]}}`, new: `"integrations":null`},
		{name: "symphony", old: `"symphony":{"events":["after_create"]}`, new: `"symphony":null`},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			raw := strings.Replace(base, replacement.old, replacement.new, 1)
			if _, err := DecodeLaunchIntentV1([]byte(raw)); err == nil || !strings.Contains(err.Error(), "must not be null") {
				t.Fatalf("DecodeLaunchIntentV1 error = %v", err)
			}
		})
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

func TestLaunchIntentV1InitialInputStrictShape(t *testing.T) {
	base := `{"intent_version":1,"participants":[{"handle":"codex","runnable":true,"executable":"codex","cwd":{"kind":"relative","path":"."},"resume_policy":"fresh","execution":{"require_wake":false,"no_gitignore":false,"wake":{"mode":"enabled"}},"initial_input":%s}]}`
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "null", input: `null`, want: "must not be null"},
		{name: "unknown kind", input: `{"kind":"pipe","text":"hello"}`, want: "invalid kind"},
		{name: "missing text", input: `{"kind":"stdin"}`, want: "requires text"},
		{name: "unknown field", input: `{"kind":"file","text":"hello","path":"/tmp/pwn"}`, want: "unknown field"},
		{name: "nul", input: `{"kind":"argument","text":"hello\u0000world"}`, want: "without NUL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeLaunchIntentV1([]byte(fmt.Sprintf(base, test.input)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeLaunchIntentV1 error = %v, want %q", err, test.want)
			}
		})
	}
	for _, kind := range []string{"argument", "stdin", "file"} {
		raw := fmt.Sprintf(base, fmt.Sprintf(`{"kind":%q,"text":"hello"}`, kind))
		if _, err := DecodeLaunchIntentV1([]byte(raw)); err != nil {
			t.Fatalf("kind %s rejected: %v", kind, err)
		}
	}
}

func TestLaunchIntentV1MarshalRoundTrip(t *testing.T) {
	intent, err := DecodeLaunchIntentV1([]byte(validIntentJSON(t)))
	if err != nil {
		t.Fatal(err)
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
}
