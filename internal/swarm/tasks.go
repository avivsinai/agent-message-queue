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

// TaskList represents all tasks for a team, supporting both single-file and
// directory-based storage formats.
type TaskList struct {
	Tasks []Task `json:"tasks"`
}

// ListTasks reads all tasks for a team.
// Supports two formats:
//   - Single file: ~/.claude/tasks/{team}/tasks.json
//   - Directory: ~/.claude/tasks/{team}/*.json (one file per task)
func ListTasks(teamName string) ([]Task, error) {
	tasksDir := TeamTasksDir(teamName)

	// Try single-file format first
	singleFile := filepath.Join(tasksDir, "tasks.json")
	if data, err := os.ReadFile(singleFile); err == nil {
		var tl TaskList
		if err := json.Unmarshal(data, &tl); err != nil {
			return nil, fmt.Errorf("parse tasks.json: %w", err)
		}
		return tl.Tasks, nil
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
		var tl TaskList
		if err := json.Unmarshal(data, &tl); err == nil && len(tl.Tasks) > 0 {
			tasks = append(tasks, tl.Tasks...)
		}
	}

	return tasks, nil
}

// ClaimTask assigns a task to an agent. Uses file locking to prevent races.
func ClaimTask(teamName, taskID, agentID string) error {
	return updateTask(teamName, taskID, func(task *Task) error {
		if task.Status == TaskStatusCompleted {
			return fmt.Errorf("task %q is already completed", taskID)
		}
		if task.AssignedTo != "" && task.AssignedTo != agentID {
			return fmt.Errorf("task %q is already assigned to %q", taskID, task.AssignedTo)
		}
		task.Status = TaskStatusInProgress
		task.AssignedTo = agentID
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

// CompleteTask marks a task as completed.
func CompleteTask(teamName, taskID, agentID string) error {
	return updateTask(teamName, taskID, func(task *Task) error {
		if task.AssignedTo != "" && task.AssignedTo != agentID {
			return fmt.Errorf("task %q is assigned to %q, not %q", taskID, task.AssignedTo, agentID)
		}
		task.Status = TaskStatusCompleted
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

// updateTask finds a task by ID, applies a mutation, and writes back.
func updateTask(teamName, taskID string, mutate func(*Task) error) error {
	tasksDir := TeamTasksDir(teamName)

	// Try single-file format first
	singleFile := filepath.Join(tasksDir, "tasks.json")
	if data, err := os.ReadFile(singleFile); err == nil {
		var tl TaskList
		if err := json.Unmarshal(data, &tl); err != nil {
			return fmt.Errorf("parse tasks.json: %w", err)
		}
		found := false
		for i := range tl.Tasks {
			if tl.Tasks[i].ID == taskID {
				if err := mutate(&tl.Tasks[i]); err != nil {
					return err
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("task %q not found in team %q", taskID, teamName)
		}
		return writeTaskList(singleFile, tl)
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

		var task Task
		if err := json.Unmarshal(data, &task); err != nil || task.ID != taskID {
			continue
		}

		if err := mutate(&task); err != nil {
			return err
		}
		return writeTaskFile(path, task)
	}

	return fmt.Errorf("task %q not found in team %q", taskID, teamName)
}

func writeTaskList(path string, tl TaskList) error {
	data, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal task list: %w", err)
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write task list: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename task list: %w", err)
	}
	return nil
}

func writeTaskFile(path string, task Task) error {
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write task: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename task: %w", err)
	}
	return nil
}
