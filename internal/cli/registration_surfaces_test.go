package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func writeTestInboxMessage(t *testing.T, root, handle, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, handle), name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// who and session list list the base queue's own mailboxes, a session whose
// name starts with "_", and a mailbox without an inbox; existing fields keep
// their types (bead agent-message-queue-t97.2, Pro findings 2 and 5;
// research 1d: a sessionless root showed "No sessions found").
func TestWhoAndSessionListShowEveryMailbox(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	writeTestRoster(t, base, `{"agents":["claude"]}`)
	if err := fsq.EnsureAgentDirs(base, "lead"); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(base, "_scratch")
	if err := fsq.EnsureAgentDirs(scratch, "claude"); err != nil {
		t.Fatal(err)
	}
	writeTestIdentity(t, scratch, "reviewer", validTestIdentity("reviewer"))
	if err := os.MkdirAll(filepath.Join(scratch, "agents", "partial"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := captureEnvStdout(t, func() error { return runWho([]string{"--root", base, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	type whoAgent struct {
		Handle             string             `json:"handle"`
		Kind               string             `json:"kind"`
		RegistrationStatus string             `json:"registration_status"`
		Identity           *agentIdentityJSON `json:"identity"`
		MailboxStatus      string             `json:"mailbox_status"`
	}
	var sessions []struct {
		Name   string     `json:"name"`
		Scope  string     `json:"scope"`
		Agents []whoAgent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("who json: %v: %s", err, out)
	}
	if len(sessions) != 2 || sessions[0].Scope != "base" || sessions[0].Name != "" || sessions[1].Name != "_scratch" {
		t.Fatalf("who scopes = %+v, want the base queue then _scratch", sessions)
	}
	if got := sessions[0].Agents; len(got) != 1 || got[0].Handle != "lead" || got[0].RegistrationStatus != "unregistered" {
		t.Fatalf("base agents = %+v, want unregistered lead", got)
	}
	want := map[string]whoAgent{
		"claude":   {Handle: "claude", Kind: "agent", RegistrationStatus: "declared", MailboxStatus: "complete"},
		"partial":  {Handle: "partial", Kind: "agent", RegistrationStatus: "unregistered", MailboxStatus: "incomplete"},
		"reviewer": {Handle: "reviewer", Kind: "agent", RegistrationStatus: "observed", MailboxStatus: "complete"},
	}
	if len(sessions[1].Agents) != len(want) {
		t.Fatalf("_scratch agents = %+v", sessions[1].Agents)
	}
	for _, got := range sessions[1].Agents {
		identity := got.Identity
		got.Identity = nil
		if got != want[got.Handle] || (got.Handle == "reviewer") != (identity != nil && identity.Harness == "claude") {
			t.Fatalf("who agent = %+v identity %+v, want %+v", got, identity, want[got.Handle])
		}
	}

	out, err = captureEnvStdout(t, func() error { return runSessionList([]string{"--root", base, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Sessions []struct {
			Name         string   `json:"name"`
			Agents       []string `json:"agents"`
			AgentDetails []struct {
				Handle             string `json:"handle"`
				RegistrationStatus string `json:"registration_status"`
			} `json:"agent_details"`
		} `json:"sessions"`
		Base *struct {
			Agents []string `json:"agents"`
		} `json:"base"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("session list json: %v: %s", err, out)
	}
	if len(list.Sessions) != 1 || strings.Join(list.Sessions[0].Agents, ",") != "claude,partial,reviewer" ||
		len(list.Sessions[0].AgentDetails) != 3 || list.Sessions[0].AgentDetails[2].RegistrationStatus != "observed" {
		t.Fatalf("session list = %s", out)
	}
	if list.Base == nil || strings.Join(list.Base.Agents, ",") != "lead" {
		t.Fatalf("session list base = %s, want the base queue's lead", out)
	}
}

// presence list shows a mailbox that has no presence file, and presence set
// no longer warns about the caller's own handle; --strict still refuses
// (bead agent-message-queue-t97.2, Pro findings 2 and 5; research 1d).
func TestPresenceListShowsEveryMailbox(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude")
	writeTestIdentity(t, root, "reviewer", validTestIdentity("reviewer"))
	if err := fsq.EnsureAgentDirs(root, "quiet"); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := captureEnvOutput(t, func() error {
		return runPresenceSet([]string{"--root", root, "--me", "reviewer", "--status", "busy"})
	})
	if err != nil || stderr != "" {
		t.Fatalf("presence set: err=%v stderr=%q, want silent success", err, stderr)
	}
	if err := runPresenceSet([]string{"--root", root, "--me", "reviewer", "--status", "busy", "--strict"}); err == nil {
		t.Fatal("strict presence set for an unrostered handle succeeded")
	}

	out, err := captureEnvStdout(t, func() error { return runPresenceList([]string{"--root", root, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Handle             string `json:"handle"`
		Status             string `json:"status"`
		Kind               string `json:"kind"`
		RegistrationStatus string `json:"registration_status"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("presence list json: %v: %s", err, out)
	}
	got := map[string]string{}
	for _, item := range items {
		got[item.Handle] = item.Status + "/" + item.Kind + "/" + item.RegistrationStatus
	}
	if got["reviewer"] != "busy/agent/observed" || got["quiet"] != "unknown/agent/unregistered" || got["claude"] != "unknown/agent/declared" {
		t.Fatalf("presence list = %v", got)
	}
}

// doctor --ops reports an unrostered mailbox's backlog, a handle that exists
// only in a sibling session, and an uninspectable inbox as an error instead
// of zero; doctor mailboxes carry registration (bead agent-message-queue-t97.2,
// Pro findings 2 and 3; research 1a: reviewer's 2 unread were missing).
func TestDoctorOpsReportsEveryMailbox(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	writeTestRoster(t, base, `{"agents":["claude","codex"]}`)
	collab := filepath.Join(base, "collab")
	feat := filepath.Join(base, "feat")
	for _, mailbox := range []struct{ root, handle string }{{collab, "claude"}, {collab, "reviewer"}, {collab, "partial"}, {feat, "solo"}} {
		if err := fsq.EnsureAgentDirs(mailbox.root, mailbox.handle); err != nil {
			t.Fatal(err)
		}
	}
	writeTestInboxMessage(t, collab, "reviewer", "1.md")
	writeTestInboxMessage(t, collab, "reviewer", "2.md")
	writeTestInboxMessage(t, feat, "solo", "1.md")
	if err := os.RemoveAll(fsq.AgentInboxNew(collab, "partial")); err != nil {
		t.Fatal(err)
	}

	out, err := captureEnvStdout(t, func() error { return runDoctor([]string{"--root", collab, "--ops", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Mailboxes []struct {
			Handle             string `json:"handle"`
			RegistrationStatus string `json:"registration_status"`
		} `json:"mailboxes"`
		Ops struct {
			Agents []struct {
				Handle             string `json:"handle"`
				UnreadCount        int    `json:"unread_count"`
				UnreadCountError   string `json:"unread_count_error"`
				RegistrationStatus string `json:"registration_status"`
			} `json:"agents"`
			Hints []opsHint `json:"hints"`
		} `json:"ops"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("doctor json: %v: %s", err, out)
	}
	rows := map[string]string{}
	for _, agent := range result.Ops.Agents {
		rows[agent.Handle] = agent.RegistrationStatus
		switch agent.Handle {
		case "reviewer":
			if agent.UnreadCount != 2 || agent.UnreadCountError != "" {
				t.Fatalf("reviewer row = %+v, want 2 unread", agent)
			}
		case "partial":
			if agent.UnreadCountError == "" {
				t.Fatalf("partial row = %+v, want an unread count error", agent)
			}
		case "codex":
			if agent.UnreadCount != 0 || agent.UnreadCountError != "" {
				t.Fatalf("codex row = %+v, want a declared row with no mailbox and zero unread", agent)
			}
		}
	}
	if rows["claude"] != "declared" || rows["codex"] != "declared" || rows["reviewer"] != "unregistered" || rows["partial"] != "unregistered" {
		t.Fatalf("ops agents = %v", rows)
	}
	sibling := false
	for _, hint := range result.Ops.Hints {
		sibling = sibling || (hint.Code == "sibling_backlog" && strings.Contains(hint.Message, `"solo"`))
	}
	if !sibling {
		t.Fatalf("hints = %+v, want a sibling backlog for solo", result.Ops.Hints)
	}
	mailboxes := map[string]string{}
	for _, mailbox := range result.Mailboxes {
		mailboxes[mailbox.Handle] = mailbox.RegistrationStatus
	}
	if mailboxes["claude"] != "declared" || mailboxes["reviewer"] != "unregistered" {
		t.Fatalf("doctor mailboxes = %v", mailboxes)
	}
}

// send and reply share one endpoint policy: base and session rosters unite,
// an observed sender is not warned about itself, reply checks its sender like
// send, and route explain names the registration without changing routable
// (bead agent-message-queue-t97.2, Pro findings 1 and 4; research 1b, 1e).
func TestSendReplyEndpointRegistration(t *testing.T) {
	clearSendMailboxTestEnv(t)
	base := filepath.Join(t.TempDir(), ".agent-mail")
	writeTestRoster(t, base, `{"agents":["claude","codex"]}`)
	feat := filepath.Join(base, "feat")
	writeTestRoster(t, feat, `{"agents":["reviewer"]}`)
	for _, handle := range []string{"claude", "reviewer", "ghost"} {
		if err := fsq.EnsureAgentDirs(feat, handle); err != nil {
			t.Fatal(err)
		}
	}
	writeTestIdentity(t, feat, "helper", validTestIdentity("helper"))

	send := func(me, to string) (string, string) {
		t.Helper()
		out, stderr, err := captureEnvOutput(t, func() error {
			return runSend([]string{"--root", feat, "--me", me, "--to", to, "--body", "hi", "--json"})
		})
		if err != nil {
			t.Fatalf("send %s->%s: %v", me, to, err)
		}
		var sent struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &sent); err != nil {
			t.Fatalf("send json: %v: %s", err, out)
		}
		return sent.ID, stderr
	}
	if _, stderr := send("claude", "reviewer"); stderr != "" {
		t.Fatalf("send to a session-declared handle warned: %q", stderr)
	}
	if _, stderr := send("helper", "claude"); stderr != "" {
		t.Fatalf("send from an observed handle warned: %q", stderr)
	}
	id, stderr := send("claude", "ghost")
	if !strings.Contains(stderr, `unregistered recipient "ghost"`) {
		t.Fatalf("send to an unregistered handle: stderr %q", stderr)
	}
	_, stderr, err := captureEnvOutput(t, func() error {
		return runReply([]string{"--root", feat, "--me", "ghost", "--id", id, "--body", "back"})
	})
	if err != nil || !strings.Contains(stderr, `unregistered sender "ghost"`) || strings.Contains(stderr, "recipient") {
		t.Fatalf("reply from an unregistered handle: err=%v stderr=%q", err, stderr)
	}

	for handle, want := range map[string]registrationStatus{"reviewer": registrationDeclared, "ghost": registrationUnregistered} {
		result := explainRoute(routeExplainOptions{To: handle, FromRoot: feat, Me: "claude"})
		if !result.Routable || result.Registration != want {
			t.Fatalf("route explain %s = routable %v registration %q (%s), want routable %q", handle, result.Routable, result.Registration, result.Error, want)
		}
	}
}
