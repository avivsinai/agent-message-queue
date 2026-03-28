package kanban

const (
	kanbanEventSnapshot              = "snapshot"
	kanbanEventWorkspaceStateUpdated = "workspace_state_updated"
	kanbanEventTaskSessionsUpdated   = "task_sessions_updated"
	kanbanEventTaskReadyForReview    = "task_ready_for_review"
	kanbanEventTaskChatMessage       = "task_chat_message"

	sessionStateIdle           = "idle"
	sessionStateRunning        = "running"
	sessionStateAwaitingReview = "awaiting_review"
	sessionStateFailed         = "failed"
	sessionStateInterrupted    = "interrupted"
)

type runtimeEnvelope struct {
	Type string `json:"type"`
}

type snapshotMessage struct {
	Type             string          `json:"type"`
	CurrentProjectID string          `json:"currentProjectId"`
	WorkspaceState   *workspaceState `json:"workspaceState"`
}

type workspaceStateUpdatedMessage struct {
	Type           string         `json:"type"`
	WorkspaceID    string         `json:"workspaceId"`
	WorkspaceState workspaceState `json:"workspaceState"`
}

type taskSessionsUpdatedMessage struct {
	Type        string               `json:"type"`
	WorkspaceID string               `json:"workspaceId"`
	Summaries   []runtimeTaskSession `json:"summaries"`
}

type taskReadyForReviewMessage struct {
	Type        string `json:"type"`
	WorkspaceID string `json:"workspaceId"`
	TaskID      string `json:"taskId"`
	TriggeredAt int64  `json:"triggeredAt"`
}

type workspaceState struct {
	RepoPath string                        `json:"repoPath"`
	Board    boardData                     `json:"board"`
	Sessions map[string]runtimeTaskSession `json:"sessions"`
}

type boardData struct {
	Columns []boardColumn `json:"columns"`
}

type boardColumn struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Cards []boardCard `json:"cards"`
}

type boardCard struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

type runtimeTaskSession struct {
	TaskID        string  `json:"taskId"`
	State         string  `json:"state"`
	AgentID       *string `json:"agentId"`
	WorkspacePath *string `json:"workspacePath"`
	ReviewReason  *string `json:"reviewReason"`
	UpdatedAt     int64   `json:"updatedAt"`
}

type cardMeta struct {
	TaskID        string
	Prompt        string
	Column        string
	WorkspaceID   string
	WorkspacePath string
}

type taskSessionSummary struct {
	TaskID        string
	State         string
	AgentID       string
	WorkspacePath string
	ReviewReason  string
	UpdatedAt     int64
}

type bridgeState struct {
	cardsByTaskID    map[string]cardMeta
	sessionsByTaskID map[string]taskSessionSummary
	handoffSent      map[string]bool
}

func newBridgeState() *bridgeState {
	return &bridgeState{
		cardsByTaskID:    make(map[string]cardMeta),
		sessionsByTaskID: make(map[string]taskSessionSummary),
		handoffSent:      make(map[string]bool),
	}
}

func (s *bridgeState) reset() {
	s.cardsByTaskID = make(map[string]cardMeta)
	s.sessionsByTaskID = make(map[string]taskSessionSummary)
	s.handoffSent = make(map[string]bool)
}

func (s *bridgeState) bootstrap(snapshot snapshotMessage) {
	s.reset()
	if snapshot.WorkspaceState == nil {
		return
	}

	s.refreshWorkspace(snapshot.CurrentProjectID, snapshot.WorkspaceState)
	for taskID, raw := range snapshot.WorkspaceState.Sessions {
		summary := summaryFromRuntime(taskID, raw, snapshot.WorkspaceState.RepoPath)
		if summary.TaskID == "" {
			continue
		}
		s.sessionsByTaskID[summary.TaskID] = summary
	}
}

func (s *bridgeState) refreshWorkspace(workspaceID string, state *workspaceState) {
	if state == nil {
		return
	}

	for taskID, meta := range s.cardsByTaskID {
		if meta.WorkspaceID == workspaceID {
			delete(s.cardsByTaskID, taskID)
		}
	}

	for _, column := range state.Board.Columns {
		for _, card := range column.Cards {
			if card.ID == "" {
				continue
			}
			s.cardsByTaskID[card.ID] = cardMeta{
				TaskID:        card.ID,
				Prompt:        card.Prompt,
				Column:        column.ID,
				WorkspaceID:   workspaceID,
				WorkspacePath: state.RepoPath,
			}
		}
	}
}

func (s *bridgeState) applyTaskSessions(msg taskSessionsUpdatedMessage) []bridgeNotification {
	notifications := make([]bridgeNotification, 0, len(msg.Summaries))
	for _, raw := range msg.Summaries {
		summary := summaryFromRuntime(raw.TaskID, raw, "")
		if summary.TaskID == "" {
			continue
		}

		previous, known := s.sessionsByTaskID[summary.TaskID]
		meta := s.metaForTask(summary.TaskID, msg.WorkspaceID)

		if summary.State != sessionStateAwaitingReview {
			delete(s.handoffSent, summary.TaskID)
		}

		if !known || previous.State != summary.State {
			switch summary.State {
			case sessionStateRunning:
				notifications = append(notifications, runningNotification(msg.WorkspaceID, summary, meta))
			case sessionStateFailed, sessionStateInterrupted:
				notifications = append(notifications, blockingNotification(msg.WorkspaceID, summary, meta))
			case sessionStateAwaitingReview:
				if !s.handoffSent[summary.TaskID] {
					notifications = append(notifications, handoffNotification(kanbanEventTaskSessionsUpdated, msg.WorkspaceID, summary, meta))
					s.handoffSent[summary.TaskID] = true
				}
			}
		}

		s.sessionsByTaskID[summary.TaskID] = summary
	}
	return notifications
}

func (s *bridgeState) applyTaskReadyForReview(msg taskReadyForReviewMessage) *bridgeNotification {
	if msg.TaskID == "" || s.handoffSent[msg.TaskID] {
		return nil
	}

	s.handoffSent[msg.TaskID] = true
	summary, ok := s.sessionsByTaskID[msg.TaskID]
	if !ok {
		summary = taskSessionSummary{
			TaskID: msg.TaskID,
			State:  sessionStateAwaitingReview,
		}
	} else {
		summary.State = sessionStateAwaitingReview
	}
	meta := s.metaForTask(msg.TaskID, msg.WorkspaceID)
	return ptrNotification(handoffNotification(kanbanEventTaskReadyForReview, msg.WorkspaceID, summary, meta))
}

func (s *bridgeState) metaForTask(taskID, workspaceID string) cardMeta {
	meta, ok := s.cardsByTaskID[taskID]
	if ok {
		return meta
	}
	return cardMeta{
		TaskID:      taskID,
		WorkspaceID: workspaceID,
	}
}

func summaryFromRuntime(taskID string, raw runtimeTaskSession, fallbackWorkspacePath string) taskSessionSummary {
	if raw.TaskID != "" {
		taskID = raw.TaskID
	}
	return taskSessionSummary{
		TaskID:        taskID,
		State:         raw.State,
		AgentID:       derefString(raw.AgentID),
		WorkspacePath: firstNonEmpty(derefString(raw.WorkspacePath), fallbackWorkspacePath),
		ReviewReason:  derefString(raw.ReviewReason),
		UpdatedAt:     raw.UpdatedAt,
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func ptrNotification(note bridgeNotification) *bridgeNotification {
	return &note
}
