package launch

import (
	"errors"
	"strings"
	"testing"
)

func TestProjectConfigRoundTripAndStrictAuthority(t *testing.T) {
	cfg := ProjectConfig{
		Schema: ProjectConfigSchema, DefaultSession: "collab",
		Agents: []ProjectAgentConfig{
			{Handle: "claude", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: ResumeEnabled},
			{Handle: "codex", Adapter: "codex", Command: []string{"codex"}, ResumePolicy: ResumeDisabled},
		},
		Layout: LayoutIntent{Type: LayoutColumns},
	}
	data, err := MarshalProjectConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalProjectConfig: %v", err)
	}
	parsed, err := ParseProjectConfig(data)
	if err != nil {
		t.Fatalf("ParseProjectConfig: %v", err)
	}
	if parsed.DefaultSession != "collab" || len(parsed.Agents) != 2 {
		t.Fatalf("parsed config = %#v", parsed)
	}
	for _, hostile := range []string{
		`{"schema":1,"default_session":"collab","agents":[{"handle":"claude","adapter":"claude","command":["bash","./agent"],"resume_policy":"resume"}],"layout":{"type":"columns"}}`,
		`{"schema":1,"default_session":"collab","agents":[{"handle":"cursor","adapter":"cursor-agent","command":["/tmp/agent"],"resume_policy":"resume"}],"layout":{"type":"columns"}}`,
		`{"schema":1,"default_session":"collab","agents":[{"handle":"claude","adapter":"claude","command":["claude"],"resume_policy":"resume"}],"layout":{"type":"columns"},"root":"elsewhere"}`,
		`{"schema":1,"default_session":"../escape","agents":[{"handle":"claude","adapter":"claude","command":["claude"],"resume_policy":"resume"}],"layout":{"type":"columns"}}`,
	} {
		if _, err := ParseProjectConfig([]byte(hostile)); err == nil {
			t.Fatalf("hostile config accepted: %s", hostile)
		}
	}
}

func TestProjectConfigNormalizesLegacyMinimalRoster(t *testing.T) {
	parsed, err := ParseProjectConfig([]byte(`{"schema":1,"agents":[{"handle":"claude","command":["claude"]}]}`))
	if err != nil {
		t.Fatalf("ParseProjectConfig: %v", err)
	}
	if parsed.DefaultSession != DefaultSessionName || parsed.Layout.Type != LayoutColumns {
		t.Fatalf("normalized project defaults = %#v", parsed)
	}
	agent := parsed.Agents[0]
	if agent.Adapter != "claude" || agent.ResumePolicy != ResumeEnabled {
		t.Fatalf("normalized agent defaults = %#v", agent)
	}
}

func TestProjectConfigAcceptsCursorCurrentExecutableAlias(t *testing.T) {
	parsed, err := ParseProjectConfig([]byte(`{"schema":1,"default_session":"collab","agents":[{"handle":"cursor","adapter":"cursor-agent","command":["agent"],"resume_policy":"resume"}],"layout":{"type":"columns"}}`))
	if err != nil {
		t.Fatalf("current Cursor executable alias rejected: %v", err)
	}
	if parsed.Agents[0].Adapter != CursorProvider || parsed.Agents[0].Command[0] != "agent" {
		t.Fatalf("current Cursor config = %#v", parsed.Agents[0])
	}
}

func TestProjectConfigAcceptsOptionalNamedPreference(t *testing.T) {
	parsed, err := ParseProjectConfig([]byte(`{"schema":1,"named":false,"agents":[{"handle":"claude","command":["claude"]}]}`))
	if err != nil {
		t.Fatalf("ParseProjectConfig: %v", err)
	}
	if parsed.Named == nil || *parsed.Named {
		t.Fatalf("named preference = %#v, want false", parsed.Named)
	}
	data, err := MarshalProjectConfig(parsed)
	if err != nil {
		t.Fatalf("MarshalProjectConfig: %v", err)
	}
	if !strings.Contains(string(data), `"named": false`) {
		t.Fatalf("marshaled config omitted named preference: %s", data)
	}
}

func TestProjectConfigRejectsExplicitEmptyOptionalDefaults(t *testing.T) {
	for _, raw := range []string{
		`{"schema":1,"default_session":"","agents":[{"handle":"claude","command":["claude"]}]}`,
		`{"schema":1,"default_session":null,"agents":[{"handle":"claude","command":["claude"]}]}`,
		`{"schema":1,"agents":[{"handle":"claude","adapter":"","command":["claude"]}]}`,
		`{"schema":1,"agents":[{"handle":"claude","adapter":null,"command":["claude"]}]}`,
		`{"schema":1,"agents":[{"handle":"claude","command":["claude"],"resume_policy":""}]}`,
		`{"schema":1,"agents":[{"handle":"claude","command":["claude"]}],"layout":{"type":""}}`,
	} {
		if _, err := ParseProjectConfig([]byte(raw)); err == nil {
			t.Fatalf("explicit empty default accepted: %s", raw)
		}
	}
}

func TestLocalConfigRejectsAuthorityFields(t *testing.T) {
	for _, field := range []string{"agents", "default_session", "argv", "env", "cwd", "bypass_args", "root", "named"} {
		raw := `{"schema":1,"launcher_preference":["commands"],"` + field + `":"forged"}`
		_, err := ParseLocalConfig(".amq/launch.local.json", []byte(raw))
		var conflict *ConfigAuthorityConflictError
		if !errors.As(err, &conflict) || conflict.Field != field {
			t.Fatalf("field %s error = %T %v", field, err, err)
		}
	}
	if _, err := ParseLocalConfig("local", []byte(`{"schema":1,"launcher_preference":["commands"],"unknown":true}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown local field error = %v", err)
	}
}

func TestLocalConfigRequiresCommandsFallback(t *testing.T) {
	_, err := MarshalLocalConfig(LocalConfig{Schema: LocalConfigSchema, LauncherPreference: []string{LauncherTMux}})
	if err == nil || !strings.Contains(err.Error(), LauncherCommands) {
		t.Fatalf("error = %v", err)
	}
}
