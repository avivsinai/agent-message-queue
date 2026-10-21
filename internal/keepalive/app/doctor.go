package app

import (
	"flag"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

func (a App) doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	registryPath := fs.String("registry", mustDefaultRegistryPath(), "registry file path")
	ops := fs.Bool("ops", false, "list companion (adapter=remote) rows with lifetime-lock probe status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := resolveDefaultRegistry(registryPath); err != nil {
		return err
	}
	store := registry.New(*registryPath)
	file, err := store.Load()
	if err != nil {
		return err
	}
	if *ops {
		return printJSON(a.Stdout, companionOpsReport(*registryPath, file))
	}
	settingsPath, err := keepaliveSettingsPath(*registryPath)
	if err != nil {
		return err
	}
	return printJSON(a.Stdout, doctorReport{File: file, SelfUpgrade: doctorSelfUpgradeSetting(settingsPath)})
}

// doctorReport is the registry file plus the self_upgrade setting a
// supervisor using this registry reads. The setting is the file's value; a
// supervisor started with --no-self-upgrade stays disabled whatever it says.
type doctorReport struct {
	registry.File
	SelfUpgrade doctorSetting `json:"self_upgrade"`
}

type doctorSetting struct {
	Value  bool   `json:"value"`
	Source string `json:"source"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// doctorSelfUpgradeSetting shows a refused file with the default, the value
// a starting supervisor runs with; a running one keeps its last good value.
func doctorSelfUpgradeSetting(path string) doctorSetting {
	read := readKeepaliveSettings(path)
	setting := doctorSetting{Value: true, Source: "default", Path: path, Status: keepaliveSettingsStatusOK}
	switch {
	case read.err != nil:
		setting.Status, setting.Error = keepaliveSettingsStatusRefused, read.err.Error()
	case !read.exists:
		setting.Status = keepaliveSettingsStatusAbsent
	case read.selfUpgradeSet:
		setting.Value, setting.Source = read.selfUpgrade, "file"
	}
	return setting
}

// companionOpsReport classifies companion-supervised rows (adapter
// "remote", registered by amq-remote up) by probing each row's
// process-lifetime lock. A row whose lock is held is live;
// a row whose lock is not held is STALE — its up died hard (kill -9)
// without Forget — and doctor must never present it as active. A probe
// error is surfaced as "unknown" and counts as a warning in the exit
// code, never silently as active.
func companionOpsReport(regPath string, file registry.File) map[string]any {
	type row struct {
		ID       string `json:"id"`
		Root     string `json:"root"`
		Agent    string `json:"agent"`
		Target   string `json:"target"`
		State    string `json:"state"`
		Lock     string `json:"lock"`
		ProbeErr string `json:"probe_error,omitempty"`
	}
	report := map[string]any{"companions": []row{}, "stale": 0, "live": 0, "unknown": 0}
	companions := []row{}
	stale, live, unknown := 0, 0, 0
	for _, e := range file.Entries {
		if e.Adapter != "remote" {
			continue
		}
		r := row{ID: e.ID, Root: e.Root, Agent: e.Agent, Target: e.Target, State: string(e.State)}
		held, err := registry.ProbeLifetimeLock(regPath, e.ID)
		switch {
		case err != nil:
			r.Lock = "unknown"
			r.ProbeErr = err.Error()
			unknown++
		case held:
			r.Lock = "held"
			live++
		default:
			r.Lock = "not-held"
			r.State = "stale"
			stale++
		}
		companions = append(companions, r)
	}
	report["companions"] = companions
	report["stale"] = stale
	report["live"] = live
	report["unknown"] = unknown
	return report
}
