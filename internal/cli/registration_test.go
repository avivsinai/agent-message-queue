package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func writeTestRoster(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestIdentity(t *testing.T, root, handle, body string) {
	t.Helper()
	if err := fsq.EnsureAgentDirs(root, handle); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", handle, "identity.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validTestIdentity(handle string) string {
	return `{"schema":1,"handle":"` + handle + `","harness":"claude","observed_at":"2026-10-23T06:10:12Z"}`
}

// Registration rows from the agent identity design review (bead
// agent-message-queue-t97.1, Pro findings 1, 2, 4 and 5; research 1e).
func TestClassifyAgentRegistration(t *testing.T) {
	base := t.TempDir()
	session := filepath.Join(base, "feat")
	writeTestRoster(t, base, `{"agents":["claude","codex"]}`)
	writeTestRoster(t, session, `{"agents":["reviewer"]}`)
	for _, handle := range []string{"claude", "reviewer", "reviewr"} {
		if err := fsq.EnsureAgentDirs(session, handle); err != nil {
			t.Fatal(err)
		}
	}
	writeTestIdentity(t, session, "claude", validTestIdentity("claude"))
	writeTestIdentity(t, session, "observer", validTestIdentity("observer"))
	writeTestIdentity(t, session, "corrupt", `{"schema":1,"handle":"corrupt"`)
	writeTestIdentity(t, session, "renamed", validTestIdentity("someone-else"))

	corruptSession := filepath.Join(base, "broken")
	writeTestRoster(t, corruptSession, `{"agents":`)
	writeTestIdentity(t, corruptSession, "observer", validTestIdentity("observer"))

	sessionDecls := declarationsFor(session, base)
	brokenDecls := declarationsFor(corruptSession, base)
	tests := []struct {
		name         string
		decls        agentDeclarations
		root         string
		handle       string
		status       registrationStatus
		kind         string
		identity     bool
		diagnostics  int
		strictRefuse bool
	}{
		// Two rosters in one session: base and session declarations unite,
		// so neither command family contradicts the other (research 1e).
		{name: "base roster declares in session", decls: sessionDecls, root: session, handle: "codex", status: registrationDeclared, kind: "agent"},
		{name: "session roster declares", decls: sessionDecls, root: session, handle: "reviewer", status: registrationDeclared, kind: "agent"},
		// A declared handle keeps its observed identity (Pro 5).
		{name: "declared keeps identity", decls: sessionDecls, root: session, handle: "claude", status: registrationDeclared, kind: "agent", identity: true},
		// A cleaned or not-yet-joined declared handle has no mailbox and
		// stays declared (Pro 1).
		{name: "declared without mailbox", decls: declarationsFor(base, ""), root: base, handle: "codex", status: registrationDeclared, kind: "agent"},
		{name: "reserved user", decls: sessionDecls, root: session, handle: reservedHumanHandle, status: registrationDeclared, kind: "human"},
		{name: "valid identity observes", decls: sessionDecls, root: session, handle: "observer", status: registrationObserved, kind: "agent", identity: true},
		{name: "typo mailbox unregistered", decls: sessionDecls, root: session, handle: "reviewr", status: registrationUnregistered, kind: "agent"},
		// A corrupt record is a diagnostic, never "observed" (Pro 2).
		{name: "malformed identity", decls: sessionDecls, root: session, handle: "corrupt", status: registrationUnregistered, kind: "agent", diagnostics: 1},
		{name: "identity for another handle", decls: sessionDecls, root: session, handle: "renamed", status: registrationUnregistered, kind: "agent", diagnostics: 1},
		// An unreadable roster is a diagnostic, never a silent "not
		// declared", and strict callers can refuse on it (Pro 2, 4).
		{name: "unreadable roster, observed", decls: brokenDecls, root: corruptSession, handle: "observer", status: registrationObserved, kind: "agent", identity: true, diagnostics: 1, strictRefuse: true},
		{name: "unreadable roster, base still declares", decls: brokenDecls, root: corruptSession, handle: "claude", status: registrationDeclared, kind: "agent", strictRefuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAgentAt(tt.decls, tt.root, tt.handle)
			if got.Status != tt.status || got.Kind != tt.kind || (got.Identity != nil) != tt.identity || len(got.Diagnostics) != tt.diagnostics {
				t.Fatalf("classify %s = status %q kind %q identity %v diagnostics %v; want %q %q %v %d",
					tt.handle, got.Status, got.Kind, got.Identity, got.Diagnostics, tt.status, tt.kind, tt.identity, tt.diagnostics)
			}
			if refuse := tt.decls.err() != nil; refuse != tt.strictRefuse {
				t.Fatalf("declarations err = %v, want refusal %v", tt.decls.err(), tt.strictRefuse)
			}
		})
	}
}

// The inventory keeps every mailbox row: the base queue's own agents, a
// session the validator accepts with a leading underscore, an incomplete
// mailbox, a handle with no presence and one with a corrupt identity
// (bead agent-message-queue-t97.1, Pro finding 2).
func TestInventoryProjectKeepsEveryMailbox(t *testing.T) {
	base := t.TempDir()
	writeTestRoster(t, base, `{"agents":["claude"]}`)
	if err := fsq.EnsureAgentDirs(base, "lead"); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(base, "_scratch")
	if err := fsq.EnsureAgentDirs(hidden, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := presence.Write(hidden, presence.New("claude", "busy", "", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(hidden, "agents", "partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestIdentity(t, hidden, "claude", validTestIdentity("claude"))
	writeTestIdentity(t, hidden, "corrupt", `not json`)

	scopes, err := inventoryProject(base, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0].Kind != inventoryScopeBase || scopes[0].Session != "" || scopes[1].Session != "_scratch" {
		t.Fatalf("scopes = %+v, want base then _scratch", scopes)
	}
	if len(scopes[0].Agents) != 1 || scopes[0].Agents[0].Handle != "lead" || scopes[0].Agents[0].Registration.Status != registrationUnregistered {
		t.Fatalf("base agents = %+v, want unregistered lead", scopes[0].Agents)
	}
	rows := map[string]inventoryAgent{}
	for _, agent := range scopes[1].Agents {
		rows[agent.Handle] = agent
	}
	claude, partial, corrupt := rows["claude"], rows["partial"], rows["corrupt"]
	if claude.Registration.Status != registrationDeclared || claude.Presence == nil || len(claude.MailboxIssues) != 0 {
		t.Fatalf("claude = %+v, want declared via base roster with presence and complete mailbox", claude)
	}
	// The additive JSON projection keeps a declared handle's identity.
	projection, err := json.Marshal(claude.Registration.json())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"registration_status":"declared","identity":{"harness":"claude","observed_at":"2026-10-23T06:10:12Z"}}`; string(projection) != want {
		t.Fatalf("registration json = %s, want %s", projection, want)
	}
	if partial.Handle == "" || len(partial.MailboxIssues) == 0 || partial.Presence != nil {
		t.Fatalf("partial = %+v, want a row with mailbox issues and no presence", partial)
	}
	if corrupt.Handle == "" || len(corrupt.Registration.Diagnostics) != 1 || corrupt.Registration.Status != registrationUnregistered {
		t.Fatalf("corrupt = %+v, want an unregistered row with an identity diagnostic", corrupt)
	}
}

// A mailbox whose inbox is a symlink keeps its local row and reports the
// link instead of reading through it as complete (Codex review of PR #1195).
func TestInventoryReportsSymlinkedInbox(t *testing.T) {
	base := t.TempDir()
	if err := fsq.EnsureAgentDirs(base, "lead"); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "inbox")
	for _, leaf := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(foreign, leaf), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inbox := filepath.Join(base, "agents", "lead", "inbox")
	if err := os.RemoveAll(inbox); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, inbox); err != nil {
		t.Fatal(err)
	}

	scope := inventoryDeliveryRoot(base, "")
	if len(scope.Agents) != 1 || scope.Agents[0].Handle != "lead" {
		t.Fatalf("agents = %+v, want the local lead row", scope.Agents)
	}
	if issues := scope.Agents[0].MailboxIssues; len(issues) != 1 || issues[0] != "inbox is a symlink" {
		t.Fatalf("mailbox issues = %q, want [inbox is a symlink]", issues)
	}
}

// A symlinked agents/ directory must not list the mailboxes it points at as
// local; the scope carries the problem instead (Codex review of PR #1195).
func TestInventoryRefusesSymlinkedAgentsDir(t *testing.T) {
	base := t.TempDir()
	foreign := t.TempDir()
	if err := fsq.EnsureAgentDirs(foreign, "ghost"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(foreign, "agents"), filepath.Join(base, "agents")); err != nil {
		t.Fatal(err)
	}

	scopes, err := inventoryProject(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || len(scopes[0].Agents) != 0 || scopes[0].Err == nil {
		t.Fatalf("scopes = %+v, want one base scope with no agents and an error", scopes)
	}
}
