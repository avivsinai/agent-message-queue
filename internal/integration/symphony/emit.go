package symphony

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/integration/common"
)

// Valid symphony lifecycle events, matching the spec.
var ValidEvents = []string{"after_create", "before_run", "after_run", "before_remove"}

// EmitOptions configures the Emit operation.
type EmitOptions struct {
	Event      string // Lifecycle event name (required)
	Me         string // Agent handle (required)
	Root       string // AMQ root directory (required, resolved)
	Workspace  string // Workspace path (default: cwd)
	Identifier string // Workspace key (default: basename of workspace)
}

// EmitResult describes the outcome of an Emit operation.
type EmitResult struct {
	Event       string `json:"event"`
	Me          string `json:"me"`
	Workspace   string `json:"workspace"`
	Identifier  string `json:"identifier"`
	Thread      string `json:"thread"`
	MessagePath string `json:"message_path"`
}

// Emit builds and delivers an AMQ message for a symphony lifecycle event.
//
// The message is self-delivered: from=me, to=me. This allows an agent
// monitoring its own inbox to react to orchestrator events.
func Emit(opts EmitOptions) (*EmitResult, error) {
	if err := validateEvent(opts.Event); err != nil {
		return nil, err
	}

	// Resolve workspace path
	workspace := opts.Workspace
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
	}
	workspace, _ = filepath.Abs(workspace)

	// Resolve identifier
	identifier := opts.Identifier
	if identifier == "" {
		identifier = filepath.Base(workspace)
	}
	// Sanitize identifier for use as thread component
	identifier = sanitizeIdentifier(identifier)

	// Build thread per spec: "task/<workspace_key>"
	thread := "task/" + identifier

	// Determine task state from event (when confidently known)
	taskState := eventToTaskState(opts.Event)

	// Build orchestrator context per spec
	task := map[string]interface{}{
		"id": identifier,
	}
	if taskState != "" {
		task["state"] = taskState
	}

	ctx := common.BuildOrchestratorContext(
		"symphony",
		"hook",
		opts.Event,
		&common.WorkspaceContext{
			Path: workspace,
			Key:  identifier,
		},
		task,
	)

	// Build labels per spec
	var extraLabels []string
	if taskState == "awaiting_review" {
		extraLabels = append(extraLabels, "handoff")
	}
	labels := common.BuildOrchestratorLabels("symphony", taskState, extraLabels...)

	// Build message content
	subject := fmt.Sprintf("[symphony] %s: %s", opts.Event, identifier)
	body := fmt.Sprintf("Event: %s\nWorkspace: %s\nIdentifier: %s\n", opts.Event, workspace, identifier)

	// Determine kind and priority
	kind := format.KindStatus
	priority := format.PriorityLow

	// after_run and before_remove are more significant
	switch opts.Event {
	case "after_run":
		kind = format.KindStatus
		priority = format.PriorityNormal
	case "before_remove":
		kind = format.KindStatus
		priority = format.PriorityNormal
	}

	// Deliver to self
	msgPath, err := common.DeliverIntegrationMessage(
		opts.Root, opts.Me, opts.Me,
		subject, body, ctx, labels,
		thread, kind, priority,
	)
	if err != nil {
		return nil, fmt.Errorf("deliver symphony event: %w", err)
	}

	return &EmitResult{
		Event:       opts.Event,
		Me:          opts.Me,
		Workspace:   workspace,
		Identifier:  identifier,
		Thread:      thread,
		MessagePath: msgPath,
	}, nil
}

// validateEvent checks that the event name is valid.
func validateEvent(event string) error {
	for _, v := range ValidEvents {
		if event == v {
			return nil
		}
	}
	return fmt.Errorf("invalid event %q: must be one of %s", event, strings.Join(ValidEvents, ", "))
}

// eventToTaskState maps symphony lifecycle events to task states
// when the state can be confidently inferred.
func eventToTaskState(event string) string {
	switch event {
	case "after_create":
		return "created"
	case "before_run":
		return "running"
	case "after_run":
		return "completed"
	case "before_remove":
		return "removing"
	default:
		return ""
	}
}

// sanitizeIdentifier normalizes a workspace key for use in thread IDs
// and task identifiers. Replaces spaces and special chars with hyphens.
func sanitizeIdentifier(id string) string {
	id = strings.TrimSpace(id)
	var sb strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			sb.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			sb.WriteRune(r + 32) // lowercase
		case r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r == '-' || r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	return sb.String()
}
