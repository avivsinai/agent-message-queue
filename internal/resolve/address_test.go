// internal/resolve/address_test.go
package resolve

import (
	"testing"
)

func TestParseAddress_BareHandle(t *testing.T) {
	ep, err := ParseAddress("codex")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "codex" || ep.Session != "" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_AgentAtSession(t *testing.T) {
	ep, err := ParseAddress("codex@auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "codex" || ep.Session != "auth" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_AgentAtProjectSession(t *testing.T) {
	ep, err := ParseAddress("claude@infra-lib:auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindAgent || ep.Agent != "claude" || ep.Project != "infra-lib" || ep.Session != "auth" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

// TestParseAddress_AgentAtName tests the ambiguous agent@name form.
// Syntactically, agent@name cannot distinguish session from project.
// The parser stores it in Session (local-first convention); the resolver
// disambiguates at resolution time. Use agent@project:session for explicit
// project targeting.
func TestParseAddress_AgentAtName(t *testing.T) {
	ep, err := ParseAddress("claude@infra-lib")
	if err != nil {
		t.Fatal(err)
	}
	// Ambiguous form: stored in Session for local-first resolution.
	// The resolver will check local sessions first, then project registry.
	if ep.Kind != KindAgent || ep.Agent != "claude" || ep.Session != "infra-lib" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_Channel(t *testing.T) {
	ep, err := ParseAddress("#events")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindChannel || ep.Channel != "events" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_ChannelAtProject(t *testing.T) {
	ep, err := ParseAddress("#all@infra-lib")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindChannel || ep.Channel != "all" || ep.Project != "infra-lib" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_SessionChannel(t *testing.T) {
	ep, err := ParseAddress("#session/auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Kind != KindChannel || ep.Channel != "session" || ep.Session != "auth" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_ExplicitLongForm(t *testing.T) {
	ep, err := ParseAddress("claude@session/auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Agent != "claude" || ep.Session != "auth" || ep.Project != "" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_ExplicitProjectLongForm(t *testing.T) {
	ep, err := ParseAddress("claude@project/infra-lib/session/auth")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Agent != "claude" || ep.Project != "infra-lib" || ep.Session != "auth" {
		t.Fatalf("unexpected: %+v", ep)
	}
}

func TestParseAddress_Invalid(t *testing.T) {
	cases := []string{"", "@auth", "#", "claude@", "claude@@auth", "UPPER"}
	for _, c := range cases {
		if _, err := ParseAddress(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestEndpoint_IsLocal(t *testing.T) {
	local, _ := ParseAddress("codex")
	if !local.IsLocal() {
		t.Fatal("bare handle should be local")
	}
	cross, _ := ParseAddress("codex@auth")
	if cross.IsLocal() {
		t.Fatal("qualified address should not be local")
	}
}

func TestEndpoint_IsCrossProject(t *testing.T) {
	ep, _ := ParseAddress("claude@infra-lib:auth")
	if !ep.IsCrossProject() {
		t.Fatal("should be cross-project")
	}
	ep2, _ := ParseAddress("codex@auth")
	if ep2.IsCrossProject() {
		t.Fatal("session-only should not be cross-project")
	}
}

func TestEndpoint_String(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"codex", "codex"},
		{"codex@auth", "codex@auth"},
		{"claude@infra-lib:auth", "claude@infra-lib:auth"},
		{"#events", "#events"},
		{"#all@infra-lib", "#all@infra-lib"},
	}
	for _, c := range cases {
		ep, err := ParseAddress(c.input)
		if err != nil {
			t.Fatal(err)
		}
		if ep.String() != c.want {
			t.Errorf("String(%q) = %q, want %q", c.input, ep.String(), c.want)
		}
	}
}
