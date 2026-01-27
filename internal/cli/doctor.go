package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok, warn, error
	Message string `json:"message,omitempty"`
}

type doctorResult struct {
	Checks  []doctorCheck `json:"checks"`
	Summary struct {
		OK    int `json:"ok"`
		Warn  int `json:"warn"`
		Error int `json:"error"`
	} `json:"summary"`
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output as JSON")

	usage := usageWithFlags(fs, "amq doctor [options]",
		"Verify AMQ installation and configuration.",
		"",
		"Checks:",
		"  - Binary version and location",
		"  - .amqrc configuration",
		"  - Mailbox directory permissions",
		"  - Agent configuration (config.json)",
		"  - Skill installation (Claude Code / Codex)",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	result := doctorResult{}

	// Check 1: Binary
	result.Checks = append(result.Checks, checkBinary())

	// Check 2: .amqrc
	amqrcCheck, root := checkAmqrc()
	result.Checks = append(result.Checks, amqrcCheck)

	// Check 3: Root directory
	if root != "" {
		result.Checks = append(result.Checks, checkRootDir(root))
	}

	// Check 4: Config.json
	if root != "" {
		result.Checks = append(result.Checks, checkConfig(root))
	}

	// Check 5: Mailbox permissions
	if root != "" {
		result.Checks = append(result.Checks, checkMailboxes(root))
	}

	// Check 6: Claude Code skill
	result.Checks = append(result.Checks, checkSkill("claude"))

	// Check 7: Codex skill
	result.Checks = append(result.Checks, checkSkill("codex"))

	// Calculate summary
	for _, check := range result.Checks {
		switch check.Status {
		case "ok":
			result.Summary.OK++
		case "warn":
			result.Summary.Warn++
		case "error":
			result.Summary.Error++
		}
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, result)
	}

	// Pretty print
	if err := writeStdoutLine("AMQ Doctor"); err != nil {
		return err
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}

	statusIcons := map[string]string{"ok": "✓", "warn": "⚠", "error": "✗"}
	for _, check := range result.Checks {
		icon := statusIcons[check.Status]
		line := fmt.Sprintf("  %s %s", icon, check.Name)
		if check.Message != "" {
			line += ": " + check.Message
		}
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}

	if err := writeStdoutLine(""); err != nil {
		return err
	}

	summary := fmt.Sprintf("Summary: %d ok", result.Summary.OK)
	if result.Summary.Warn > 0 {
		summary += fmt.Sprintf(", %d warnings", result.Summary.Warn)
	}
	if result.Summary.Error > 0 {
		summary += fmt.Sprintf(", %d errors", result.Summary.Error)
	}
	return writeStdoutLine(summary)
}

func checkBinary() doctorCheck {
	check := doctorCheck{Name: "Binary"}

	path, err := os.Executable()
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("cannot determine path: %v", err)
		return check
	}

	check.Status = "ok"
	check.Message = path
	return check
}

func checkAmqrc() (doctorCheck, string) {
	check := doctorCheck{Name: ".amqrc"}

	existing, err := findAndLoadAmqrc()
	if err == errAmqrcNotFound {
		check.Status = "warn"
		check.Message = "not found (run 'amq coop init')"
		return check, ""
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("invalid: %v", err)
		return check, ""
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("root=%s (in %s)", existing.Config.Root, existing.Dir)
	return check, filepath.Join(existing.Dir, existing.Config.Root)
}

func checkRootDir(root string) doctorCheck {
	check := doctorCheck{Name: "Root directory"}

	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		check.Status = "error"
		check.Message = fmt.Sprintf("%s does not exist", root)
		return check
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("cannot stat: %v", err)
		return check
	}
	if !info.IsDir() {
		check.Status = "error"
		check.Message = fmt.Sprintf("%s is not a directory", root)
		return check
	}

	// Check permissions (should be 0700)
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		check.Status = "warn"
		check.Message = fmt.Sprintf("%s has permissive permissions (%o)", root, perm)
		return check
	}

	check.Status = "ok"
	check.Message = root
	return check
}

func checkConfig(root string) doctorCheck {
	check := doctorCheck{Name: "Config"}

	cfgPath := filepath.Join(root, "meta", "config.json")
	cfg, err := config.LoadConfig(cfgPath)
	if os.IsNotExist(err) {
		check.Status = "warn"
		check.Message = "config.json not found"
		return check
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("invalid: %v", err)
		return check
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("agents: %v", cfg.Agents)
	return check
}

func checkMailboxes(root string) doctorCheck {
	check := doctorCheck{Name: "Mailboxes"}

	agentsDir := filepath.Join(root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if os.IsNotExist(err) {
		check.Status = "warn"
		check.Message = "no agents directory"
		return check
	}
	if err != nil {
		check.Status = "error"
		check.Message = fmt.Sprintf("cannot read agents: %v", err)
		return check
	}

	var agents []string
	var issues []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agent := entry.Name()
		agents = append(agents, agent)

		// Check inbox directories exist
		inboxNew := fsq.AgentInboxNew(root, agent)
		if _, err := os.Stat(inboxNew); os.IsNotExist(err) {
			issues = append(issues, fmt.Sprintf("%s: inbox/new missing", agent))
		}
	}

	if len(agents) == 0 {
		check.Status = "warn"
		check.Message = "no agent mailboxes"
		return check
	}

	if len(issues) > 0 {
		check.Status = "warn"
		check.Message = fmt.Sprintf("%d agents, issues: %v", len(agents), issues)
		return check
	}

	check.Status = "ok"
	check.Message = fmt.Sprintf("%d agents configured", len(agents))
	return check
}

func checkSkill(agent string) doctorCheck {
	check := doctorCheck{Name: fmt.Sprintf("%s skill", agent)}

	if agent != "claude" && agent != "codex" {
		check.Status = "warn"
		check.Message = "unknown agent"
		return check
	}

	home, _ := os.UserHomeDir()
	skillDir := filepath.Join(home, "."+agent, "skills", "amq-cli")
	localSkillDir := filepath.Join("."+agent, "skills", "amq-cli")

	// Check project-local skills first, then user-level
	switch {
	case fileExists(filepath.Join(localSkillDir, "SKILL.md")):
		check.Status = "ok"
		check.Message = "installed (project-local)"

	case fileExists(filepath.Join(skillDir, "SKILL.md")):
		check.Status = "ok"
		check.Message = "installed"

	case dirExists(skillDir):
		check.Status = "warn"
		check.Message = "skill directory exists but SKILL.md missing"

	default:
		check.Status = "warn"
		check.Message = "not installed (run: npx skills add avivsinai/agent-message-queue -g -y)"
	}

	return check
}
