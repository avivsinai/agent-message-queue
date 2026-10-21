package app

import (
	"context"
	"flag"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/launchd"
)

func (a App) installLaunchd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install-launchd", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	label := fs.String("label", launchd.DefaultLabel, "launchd label")
	plistPath := fs.String("plist", "", "plist path")
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	amqPath := fs.String("amq", "amq", "amq executable path")
	self := fs.String("self", executablePath(), "amq-keepalive executable path")
	interval := fs.Duration("interval", time.Minute, "supervisor interval")
	noLoad := fs.Bool("no-load", false, "write plist without loading it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	opts := launchd.Options{
		Label:        *label,
		PlistPath:    *plistPath,
		BinaryPath:   *self,
		RegistryPath: *registryPath,
		AMQPath:      *amqPath,
		Interval:     *interval,
		Load:         !*noLoad,
	}
	normalized, err := launchd.NormalizeOptions(opts)
	if err != nil {
		return err
	}
	if err := launchd.Install(ctx, normalized); err != nil {
		return err
	}
	return printJSON(a.Stdout, map[string]any{
		"label":      normalized.Label,
		"plist":      normalized.PlistPath,
		"loaded":     normalized.Load,
		"supervisor": normalized.BinaryPath,
	})
}

func (a App) installHook(args []string) error {
	fs := flag.NewFlagSet("install-hook", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	agent := fs.String("agent", hookinstall.AgentBoth, "agent config to update: claude, codex, or both")
	scriptPath := fs.String("script", "", "installed hook script path")
	binaryPath := fs.String("bin", executablePath(), "amq-keepalive binary path")
	claudeConfig := fs.String("claude-config", "", "Claude settings.json path")
	codexConfig := fs.String("codex-config", "", "Codex hooks.json path")
	timeout := fs.Duration("timeout", hookinstall.DefaultTimeout, "self-timeout for reattach work inside the hook")
	dryRun := fs.Bool("dry-run", false, "print install plan without writing files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := hookinstall.Install(hookinstall.Options{
		Agent:        *agent,
		ScriptPath:   *scriptPath,
		BinaryPath:   *binaryPath,
		ClaudeConfig: *claudeConfig,
		CodexConfig:  *codexConfig,
		Timeout:      *timeout,
		DryRun:       *dryRun,
	})
	if err != nil {
		return err
	}
	return printJSON(a.Stdout, result)
}

func (a App) uninstallLaunchd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	label := fs.String("label", launchd.DefaultLabel, "launchd label")
	plistPath := fs.String("plist", "", "plist path")
	noUnload := fs.Bool("no-unload", false, "remove plist without bootout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := launchd.Uninstall(ctx, *label, *plistPath, !*noUnload); err != nil {
		return err
	}
	return printJSON(a.Stdout, map[string]any{"label": *label, "removed": true, "unloaded": !*noUnload})
}
