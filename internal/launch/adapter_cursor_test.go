package launch

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func cursorProbe(version string) fakeCommandProbe {
	return fakeCommandProbe{path: "/bin/cursor-agent", outputs: map[string]string{
		"--version":          version + "\n",
		"--help":             "commands: create-chat --resume [chatId] --output-format <format>",
		"create-chat --help": "Create a new chat and return its ID",
	}}
}

func TestCursorCapabilitiesRequireExactCaptureVersion(t *testing.T) {
	adapter := NewCursorAdapter("cursor-agent")
	adapter.probe = cursorProbe(cursorCaptureVersion)
	got := adapter.Capabilities(context.Background())
	if !got.Available || !got.Fresh || !got.Resume || !got.Capture || !got.PreSpawnAcquire || got.ProviderVersion != cursorCaptureVersion {
		t.Fatalf("pinned Cursor capabilities = %#v", got)
	}
	if err := ValidateAdapterCapabilities(adapter, got); err != nil {
		t.Fatal(err)
	}

	adapter.probe = cursorProbe("2026.08.12-unknown")
	got = adapter.Capabilities(context.Background())
	if !got.Available || got.Fresh || !got.Resume || got.Capture || got.PreSpawnAcquire || got.Reason != "capture_version_unsupported" {
		t.Fatalf("untested Cursor capabilities = %#v", got)
	}
}

func TestCursorPlansAcquireBeforeFreshResume(t *testing.T) {
	project, executable := testExecutable(t, CursorProvider)
	adapter := NewCursorAdapter(executable)
	request := planRequest(project, CursorProvider)
	request.CommittedArgs = []string{"--model", "auto"}
	request.BypassArgs = []string{"--force"}

	fresh, err := adapter.PlanFresh(request)
	if err != nil {
		t.Fatal(err)
	}
	wantFresh := []string{"--model", "auto", "--force", "--resume", preSpawnConversationPlaceholder}
	if !reflect.DeepEqual(fresh.Argv[1:], wantFresh) || !fresh.PreSpawnAcquire || fresh.ConversationID != "" ||
		len(fresh.DynamicArgv) != 1 || fresh.DynamicArgv[0].Kind != DynamicArgConversationID {
		t.Fatalf("fresh Cursor plan = %#v", fresh)
	}

	resume, err := adapter.PlanResume(ResumeRequest{
		PlanRequest: request, Conversation: ConversationIdentity{Provider: CursorProvider, ID: testConversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resume.PreSpawnAcquire || !reflect.DeepEqual(resume.Argv[len(resume.Argv)-2:], []string{"--resume", testConversationID}) {
		t.Fatalf("resume Cursor plan = %#v", resume)
	}
}

func TestCursorGrammarRejectsUnownedFlagsAndMultipleBypass(t *testing.T) {
	project, executable := testExecutable(t, CursorProvider)
	adapter := NewCursorAdapter(executable)
	base := planRequest(project, CursorProvider)
	tests := []struct {
		name      string
		committed []string
		bypass    []string
	}{
		{name: "positional", committed: []string{"prompt"}},
		{name: "resume", committed: []string{"--resume", testConversationID}},
		{name: "output", committed: []string{"--output-format", "json"}},
		{name: "trust", committed: []string{"--trust"}},
		{name: "two bypass", bypass: []string{"--force", "--yolo"}},
		{name: "unknown bypass", bypass: []string{"--approve-mcps"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.CommittedArgs = test.committed
			request.BypassArgs = test.bypass
			if _, err := adapter.PlanFresh(request); err == nil {
				t.Fatal("PlanFresh error = nil")
			}
		})
	}
	if err := adapter.ValidateCommittedConfig(CommittedConfigRequest{Args: []string{"--model", ""}}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("unsafe model error = %v", err)
	}
}
