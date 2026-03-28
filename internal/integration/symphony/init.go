package symphony

import (
	"fmt"
	"os"
	"strings"
)

// Managed fragment markers for AMQ hook injection into WORKFLOW.md.
const (
	ManagedBegin = "# BEGIN AMQ MANAGED"
	ManagedEnd   = "# END AMQ MANAGED"
)

// HookEvent names matching the symphony spec.
var HookEvents = []string{"after_create", "before_run", "after_run", "before_remove"}

// InitResult describes the outcome of an Init operation.
type InitResult struct {
	WorkflowPath string `json:"workflow_path"`
	Created      bool   `json:"created"`     // true if hooks section was newly created
	Updated      bool   `json:"updated"`     // true if managed fragment was written/rewritten
	AlreadyOK    bool   `json:"already_ok"`  // true if managed fragment was already present and unchanged
	CheckOnly    bool   `json:"check_only"`  // true if --check was used
	HooksFound   bool   `json:"hooks_found"` // true if AMQ managed hooks are present in the file
}

// InitOptions configures the Init operation.
type InitOptions struct {
	WorkflowPath string // Path to WORKFLOW.md (default: "WORKFLOW.md")
	Me           string // Agent handle for --me in generated hooks
	Root         string // AMQ root to pin in generated hooks (may be empty)
	Check        bool   // Inspect only, do not write
	Force        bool   // Rewrite even if fragment already present
}

// Init patches a WORKFLOW.md file with AMQ-managed hook fragments.
//
// The function is idempotent: running it twice produces the same result.
// Existing user hook content is preserved; the AMQ managed fragment is
// appended or replaced within each hook.
func Init(opts InitOptions) (*InitResult, error) {
	if opts.WorkflowPath == "" {
		opts.WorkflowPath = "WORKFLOW.md"
	}

	wf, err := ReadWorkflow(opts.WorkflowPath)
	if err != nil {
		return nil, err
	}

	hooks := wf.GetHooks()
	result := &InitResult{
		WorkflowPath: opts.WorkflowPath,
		CheckOnly:    opts.Check,
	}

	// Check current state
	result.HooksFound = hasManagedFragment(hooks.AfterCreate) &&
		hasManagedFragment(hooks.BeforeRun) &&
		hasManagedFragment(hooks.AfterRun) &&
		hasManagedFragment(hooks.BeforeRemove)

	if opts.Check {
		return result, nil
	}

	if result.HooksFound && !opts.Force {
		result.AlreadyOK = true
		return result, nil
	}

	// Generate and inject managed fragments
	for _, event := range HookEvents {
		line := generateHookLine(event, opts.Me, opts.Root)
		fragment := managedFragment(line)

		switch event {
		case "after_create":
			hooks.AfterCreate = injectFragment(hooks.AfterCreate, fragment)
		case "before_run":
			hooks.BeforeRun = injectFragment(hooks.BeforeRun, fragment)
		case "after_run":
			hooks.AfterRun = injectFragment(hooks.AfterRun, fragment)
		case "before_remove":
			hooks.BeforeRemove = injectFragment(hooks.BeforeRemove, fragment)
		}
	}

	wf.SetHooks(hooks)

	content, err := wf.MarshalWorkflow()
	if err != nil {
		return nil, fmt.Errorf("marshal workflow: %w", err)
	}

	if err := os.WriteFile(opts.WorkflowPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write workflow: %w", err)
	}

	if result.HooksFound {
		result.Updated = true
	} else {
		result.Created = true
	}
	return result, nil
}

// generateHookLine builds the amq emit command for a given hook event.
func generateHookLine(event, me, root string) string {
	var parts []string
	parts = append(parts, "amq integration symphony emit")
	parts = append(parts, "--event", event)
	parts = append(parts, "--me", me)
	if root != "" {
		parts = append(parts, "--root", root)
	}
	return strings.Join(parts, " ") + " || true"
}

// managedFragment wraps a hook line in AMQ managed markers.
func managedFragment(line string) string {
	return ManagedBegin + "\n" + line + "\n" + ManagedEnd
}

// hasManagedFragment returns true if the hook content contains the AMQ managed markers.
func hasManagedFragment(hookContent string) bool {
	return strings.Contains(hookContent, ManagedBegin) && strings.Contains(hookContent, ManagedEnd)
}

// injectFragment inserts or replaces the AMQ managed fragment in the hook content.
// Existing user content outside the managed markers is preserved.
func injectFragment(existing, fragment string) string {
	if existing == "" {
		return fragment + "\n"
	}

	// If there's already a managed fragment, replace it
	if hasManagedFragment(existing) {
		beginIdx := strings.Index(existing, ManagedBegin)
		endIdx := strings.Index(existing, ManagedEnd) + len(ManagedEnd)

		// Preserve content before and after the managed block
		before := existing[:beginIdx]
		after := existing[endIdx:]

		return before + fragment + after
	}

	// No existing fragment: append after existing content
	content := strings.TrimRight(existing, "\n")
	return content + "\n" + fragment + "\n"
}
