package kanban

import "testing"

func TestBridgeStateBootstrapSnapshot(t *testing.T) {
	state := newBridgeState()
	snapshot := snapshotMessage{
		Type:             kanbanEventSnapshot,
		CurrentProjectID: "workspace-1",
		WorkspaceState: &workspaceState{
			RepoPath: "/repo/path",
			Board: boardData{
				Columns: []boardColumn{
					{
						ID: "in_progress",
						Cards: []boardCard{
							{ID: "task-1", Prompt: "Refactor bridge"},
						},
					},
				},
			},
			Sessions: map[string]runtimeTaskSession{
				"task-1": {
					TaskID: "task-1",
					State:  sessionStateRunning,
				},
			},
		},
	}

	state.bootstrap(snapshot)

	meta, ok := state.cardsByTaskID["task-1"]
	if !ok {
		t.Fatal("cardsByTaskID missing task-1")
	}
	if meta.Prompt != "Refactor bridge" {
		t.Fatalf("Prompt = %q, want %q", meta.Prompt, "Refactor bridge")
	}
	if meta.Column != "in_progress" {
		t.Fatalf("Column = %q, want %q", meta.Column, "in_progress")
	}
	if meta.WorkspaceID != "workspace-1" {
		t.Fatalf("WorkspaceID = %q, want %q", meta.WorkspaceID, "workspace-1")
	}

	summary, ok := state.sessionsByTaskID["task-1"]
	if !ok {
		t.Fatal("sessionsByTaskID missing task-1")
	}
	if summary.State != sessionStateRunning {
		t.Fatalf("State = %q, want %q", summary.State, sessionStateRunning)
	}
}

func TestApplyTaskSessionsAwaitingReviewFallbackSuppressesReady(t *testing.T) {
	state := newBridgeState()
	state.cardsByTaskID["task-1"] = cardMeta{
		TaskID:        "task-1",
		Prompt:        "Refactor bridge",
		Column:        "review",
		WorkspaceID:   "workspace-1",
		WorkspacePath: "/repo/path",
	}
	state.sessionsByTaskID["task-1"] = taskSessionSummary{
		TaskID:        "task-1",
		State:         sessionStateRunning,
		WorkspacePath: "/repo/path",
	}

	notifications := state.applyTaskSessions(taskSessionsUpdatedMessage{
		Type:        kanbanEventTaskSessionsUpdated,
		WorkspaceID: "workspace-1",
		Summaries: []runtimeTaskSession{
			{
				TaskID:       "task-1",
				State:        sessionStateAwaitingReview,
				ReviewReason: strPtr("hook"),
			},
		},
	})
	if len(notifications) != 1 {
		t.Fatalf("len(notifications) = %d, want 1", len(notifications))
	}
	if notifications[0].State != sessionStateAwaitingReview {
		t.Fatalf("State = %q, want %q", notifications[0].State, sessionStateAwaitingReview)
	}
	if notifications[0].Event != kanbanEventTaskSessionsUpdated {
		t.Fatalf("Event = %q, want %q", notifications[0].Event, kanbanEventTaskSessionsUpdated)
	}

	ready := state.applyTaskReadyForReview(taskReadyForReviewMessage{
		Type:        kanbanEventTaskReadyForReview,
		WorkspaceID: "workspace-1",
		TaskID:      "task-1",
	})
	if ready != nil {
		t.Fatal("expected ready event to be suppressed after fallback handoff")
	}
}

func TestApplyTaskReadyForReviewSuppressesLaterFallback(t *testing.T) {
	state := newBridgeState()
	state.cardsByTaskID["task-1"] = cardMeta{
		TaskID:      "task-1",
		Prompt:      "Refactor bridge",
		Column:      "review",
		WorkspaceID: "workspace-1",
	}
	state.sessionsByTaskID["task-1"] = taskSessionSummary{
		TaskID: "task-1",
		State:  sessionStateRunning,
	}

	ready := state.applyTaskReadyForReview(taskReadyForReviewMessage{
		Type:        kanbanEventTaskReadyForReview,
		WorkspaceID: "workspace-1",
		TaskID:      "task-1",
	})
	if ready == nil {
		t.Fatal("expected ready event notification")
	}
	if ready.Event != kanbanEventTaskReadyForReview {
		t.Fatalf("Event = %q, want %q", ready.Event, kanbanEventTaskReadyForReview)
	}

	notifications := state.applyTaskSessions(taskSessionsUpdatedMessage{
		Type:        kanbanEventTaskSessionsUpdated,
		WorkspaceID: "workspace-1",
		Summaries: []runtimeTaskSession{
			{
				TaskID: "task-1",
				State:  sessionStateAwaitingReview,
			},
		},
	})
	if len(notifications) != 0 {
		t.Fatalf("len(notifications) = %d, want 0", len(notifications))
	}
}

func TestRefreshWorkspaceReplacesCardMetadata(t *testing.T) {
	state := newBridgeState()
	state.cardsByTaskID["task-1"] = cardMeta{
		TaskID:      "task-1",
		Prompt:      "Old prompt",
		Column:      "backlog",
		WorkspaceID: "workspace-1",
	}

	state.refreshWorkspace("workspace-1", &workspaceState{
		RepoPath: "/repo/path",
		Board: boardData{
			Columns: []boardColumn{
				{
					ID: "review",
					Cards: []boardCard{
						{ID: "task-1", Prompt: "New prompt"},
					},
				},
			},
		},
	})

	meta := state.cardsByTaskID["task-1"]
	if meta.Prompt != "New prompt" {
		t.Fatalf("Prompt = %q, want %q", meta.Prompt, "New prompt")
	}
	if meta.Column != "review" {
		t.Fatalf("Column = %q, want %q", meta.Column, "review")
	}
	if meta.WorkspacePath != "/repo/path" {
		t.Fatalf("WorkspacePath = %q, want %q", meta.WorkspacePath, "/repo/path")
	}
}

func strPtr(value string) *string {
	return &value
}
