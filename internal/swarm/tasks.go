package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Task status constants matching Claude Code Agent Teams.
const (
	TaskStatusPending    = "pending"
	TaskStatusInProgress = "in_progress"
	TaskStatusCompleted  = "completed"
)

// Task represents a work item in the shared task list.
// Used for reading; writes go through map[string]any to preserve unknown fields.
type Task struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status"`
	AssignedTo  string   `json:"assigned_to,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
}

// ListTasks reads all tasks for a team.
// Supports two formats:
//   - Single file: ~/.claude/tasks/{team}/tasks.json
//   - Directory: ~/.claude/tasks/{team}/*.json (one file per task)
func ListTasks(teamName string) ([]Task, error) {
	tasksDir := TeamTasksDir(teamName)

	// Try single-file format first
	singleFile := filepath.Join(tasksDir, "tasks.json")
	data, err := os.ReadFile(singleFile)
	if err == nil {
		return parseTasksFromList(data)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read tasks.json: %w", err)
	}

	// Fall back to directory format (one JSON file per task)
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tasks directory: %w", err)
	}

	var tasks []Task
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(tasksDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		// Try single task
		var task Task
		if err := json.Unmarshal(data, &task); err == nil && task.ID != "" {
			tasks = append(tasks, task)
			continue
		}

		// Try task list wrapper
		if parsed, err := parseTasksFromList(data); err == nil && len(parsed) > 0 {
			tasks = append(tasks, parsed...)
		}
	}

	return tasks, nil
}

func parseTasksFromList(data []byte) ([]Task, error) {
	var wrapper struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse task list: %w", err)
	}
	return wrapper.Tasks, nil
}

// ClaimTask assigns a task to an agent.
// It checks dependency gating: all depends_on tasks must be completed first.
func ClaimTask(teamName, taskID, agentID string) error {
	// Check dependency gating before claiming.
	tasks, err := ListTasks(teamName)
	if err != nil {
		return fmt.Errorf("check dependencies: %w", err)
	}
	statusMap := make(map[string]string, len(tasks))
	var target *Task
	for i := range tasks {
		statusMap[tasks[i].ID] = tasks[i].Status
		if tasks[i].ID == taskID {
			target = &tasks[i]
		}
	}
	if target != nil && len(target.DependsOn) > 0 {
		var blocked []string
		for _, dep := range target.DependsOn {
			if statusMap[dep] != TaskStatusCompleted {
				blocked = append(blocked, dep)
			}
		}
		if len(blocked) > 0 {
			return fmt.Errorf("task %q is blocked by incomplete dependencies: %v", taskID, blocked)
		}
	}

	return updateTaskRaw(teamName, taskID, func(raw map[string]any) error {
		status, _ := raw["status"].(string)
		if status == TaskStatusCompleted {
			return fmt.Errorf("task %q is already completed", taskID)
		}
		assigned, _ := raw["assigned_to"].(string)
		if assigned != "" && assigned != agentID {
			return fmt.Errorf("task %q is already assigned to %q", taskID, assigned)
		}
		raw["status"] = TaskStatusInProgress
		raw["assigned_to"] = agentID
		raw["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

// CompleteTask marks a task as completed.
func CompleteTask(teamName, taskID, agentID string) error {
	return updateTaskRaw(teamName, taskID, func(raw map[string]any) error {
		assigned, _ := raw["assigned_to"].(string)
		if assigned != "" && assigned != agentID {
			return fmt.Errorf("task %q is assigned to %q, not %q", taskID, assigned, agentID)
		}
		raw["status"] = TaskStatusCompleted
		raw["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

// updateTaskRaw finds a task by ID using raw JSON maps so unknown fields are preserved.
func updateTaskRaw(teamName, taskID string, mutate func(map[string]any) error) error {
	tasksDir := TeamTasksDir(teamName)

	// Try single-file format first
	singleFile := filepath.Join(tasksDir, "tasks.json")
	data, err := os.ReadFile(singleFile)
	if err == nil {
		return updateTaskInList(singleFile, data, taskID, teamName, mutate)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read tasks.json: %w", err)
	}

	// Try per-file format
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return fmt.Errorf("read tasks directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(tasksDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		id, _ := raw["id"].(string)
		if id == taskID {
			if err := mutate(raw); err != nil {
				return err
			}
			return atomicWriteJSON(path, raw)
		}

		// Try wrapper format: {"tasks": [...]}
		if tasksRaw, ok := raw["tasks"].([]any); ok {
			for _, item := range tasksRaw {
				task, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if tid, _ := task["id"].(string); tid == taskID {
					if err := mutate(task); err != nil {
						return err
					}
					return atomicWriteJSON(path, raw)
				}
			}
		}
	}

	return fmt.Errorf("task %q not found in team %q", taskID, teamName)
}

func updateTaskInList(path string, data []byte, taskID, teamName string, mutate func(map[string]any) error) error {
	var wrapper map[string]any
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("parse tasks.json: %w", err)
	}

	tasksRaw, ok := wrapper["tasks"].([]any)
	if !ok {
		return fmt.Errorf("tasks.json missing tasks array")
	}

	found := false
	for _, item := range tasksRaw {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := task["id"].(string)
		if id != taskID {
			continue
		}
		if err := mutate(task); err != nil {
			return err
		}
		found = true
		break
	}

	if !found {
		return fmt.Errorf("task %q not found in team %q", taskID, teamName)
	}

	return atomicWriteJSON(path, wrapper)
}

func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}
