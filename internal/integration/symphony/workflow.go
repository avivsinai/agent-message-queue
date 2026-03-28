package symphony

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Workflow represents a parsed WORKFLOW.md file with YAML frontmatter and
// Markdown prompt body.
type Workflow struct {
	Config map[string]interface{} // YAML frontmatter as a generic map
	Prompt string                 // Markdown body after frontmatter
	Raw    string                 // Original file content
}

// HooksConfig represents the hooks section of the WORKFLOW.md frontmatter.
type HooksConfig struct {
	AfterCreate  string `yaml:"after_create"`
	BeforeRun    string `yaml:"before_run"`
	AfterRun     string `yaml:"after_run"`
	BeforeRemove string `yaml:"before_remove"`
}

var (
	ErrNoFrontmatter    = errors.New("WORKFLOW.md has no YAML frontmatter")
	ErrInvalidYAML      = errors.New("WORKFLOW.md frontmatter is not valid YAML")
	ErrNotAMap          = errors.New("WORKFLOW.md frontmatter must be a YAML map")
	ErrWorkflowNotFound = errors.New("WORKFLOW.md not found")
)

// ParseWorkflow parses a WORKFLOW.md file from its raw content.
// The format is:
//
//	---
//	<YAML frontmatter>
//	---
//	<Markdown prompt body>
//
// If there is no frontmatter delimiter, the entire content is treated as the
// prompt body with an empty config.
func ParseWorkflow(content string) (*Workflow, error) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return &Workflow{
			Config: map[string]interface{}{},
			Prompt: strings.TrimSpace(content),
			Raw:    content,
		}, nil
	}

	// Find the closing ---
	rest := content[4:] // skip opening "---\n"

	// Handle empty frontmatter: the closing --- immediately follows the opening
	if strings.HasPrefix(rest, "---\n") || strings.HasPrefix(rest, "---\r\n") {
		promptContent := rest[4:]
		if strings.HasPrefix(rest, "---\r\n") {
			promptContent = rest[5:]
		}
		return &Workflow{
			Config: map[string]interface{}{},
			Prompt: strings.TrimSpace(promptContent),
			Raw:    content,
		}, nil
	}

	idx := strings.Index(rest, "\n---\n")
	if idx == -1 {
		// Try with \r\n
		idx = strings.Index(rest, "\r\n---\r\n")
		if idx == -1 {
			// Check if the whole file is frontmatter (no body)
			trimmed := strings.TrimSpace(rest)
			if strings.HasSuffix(trimmed, "---") {
				rest = trimmed[:len(trimmed)-3]
				idx = len(rest) // signal that we found it
			} else {
				return nil, fmt.Errorf("%w: missing closing ---", ErrInvalidYAML)
			}
		}
	}

	yamlContent := rest[:idx]
	promptStart := idx + len("\n---\n")
	prompt := ""
	if promptStart < len(rest) {
		prompt = strings.TrimSpace(rest[promptStart:])
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &config); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidYAML, err)
	}
	if config == nil {
		config = map[string]interface{}{}
	}

	return &Workflow{
		Config: config,
		Prompt: prompt,
		Raw:    content,
	}, nil
}

// ReadWorkflow reads and parses a WORKFLOW.md file from disk.
func ReadWorkflow(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrWorkflowNotFound, path)
		}
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	return ParseWorkflow(string(data))
}

// GetHooks extracts the hooks configuration from the workflow config map.
// Returns a zero-value HooksConfig if no hooks are configured.
func (w *Workflow) GetHooks() HooksConfig {
	hooksRaw, ok := w.Config["hooks"]
	if !ok {
		return HooksConfig{}
	}
	hooksMap, ok := hooksRaw.(map[string]interface{})
	if !ok {
		return HooksConfig{}
	}

	var hooks HooksConfig
	if v, ok := hooksMap["after_create"].(string); ok {
		hooks.AfterCreate = v
	}
	if v, ok := hooksMap["before_run"].(string); ok {
		hooks.BeforeRun = v
	}
	if v, ok := hooksMap["after_run"].(string); ok {
		hooks.AfterRun = v
	}
	if v, ok := hooksMap["before_remove"].(string); ok {
		hooks.BeforeRemove = v
	}
	return hooks
}

// SetHooks writes the hooks section back into the workflow config map.
func (w *Workflow) SetHooks(hooks HooksConfig) {
	hooksMap := map[string]interface{}{}
	if hooks.AfterCreate != "" {
		hooksMap["after_create"] = hooks.AfterCreate
	}
	if hooks.BeforeRun != "" {
		hooksMap["before_run"] = hooks.BeforeRun
	}
	if hooks.AfterRun != "" {
		hooksMap["after_run"] = hooks.AfterRun
	}
	if hooks.BeforeRemove != "" {
		hooksMap["before_remove"] = hooks.BeforeRemove
	}
	if len(hooksMap) > 0 {
		w.Config["hooks"] = hooksMap
	}
}

// MarshalWorkflow serializes the workflow back to WORKFLOW.md format.
func (w *Workflow) MarshalWorkflow() (string, error) {
	yamlBytes, err := yaml.Marshal(w.Config)
	if err != nil {
		return "", fmt.Errorf("marshal workflow config: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(yamlBytes)
	sb.WriteString("---\n")
	if w.Prompt != "" {
		sb.WriteString("\n")
		sb.WriteString(w.Prompt)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
