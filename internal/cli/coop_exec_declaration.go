package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// coopExecCreationDeprecated is the stable, greppable stderr line for the
// #480 v1.1 §9 deprecation wave. Behavior is unchanged this release; the next
// major release makes the same paths exit 3 with zero writes.
const coopExecCreationDeprecated = "warning: creating a missing session or root from coop exec is deprecated; use 'amq session create <name>' or 'amq init --root'. The next major release makes this exit 3."

func warnCoopExecCreationDeprecated(warned *bool) {
	if warned == nil || *warned {
		return
	}
	*warned = true
	_ = writeStderr("%s\n", coopExecCreationDeprecated)
}

func hasBootstrapSuppressingDeclaration() (bool, error) {
	_, err := findAndLoadAmqrc()
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errAmqrcNotFound) {
		// Present but unreadable or corrupt: still a declaration. Routing
		// owns the parse failure so an AM_ROOT override can proceed.
		return true, nil
	}
	_, present, err := findProjectLaunchJSONPath()
	return present, err
}

func declaredCoopExecSession() (string, error) {
	cfg, present, err := loadProjectLaunchConfig()
	if err != nil {
		return "", err
	}
	if !present {
		return defaultSessionName, nil
	}
	return cfg.DefaultSession, nil
}

func loadProjectLaunchConfig() (launch.ProjectConfig, bool, error) {
	path, present, err := findProjectLaunchJSONPath()
	if err != nil || !present {
		return launch.ProjectConfig{}, present, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return launch.ProjectConfig{}, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	cfg, err := launch.ParseProjectConfig(data)
	if err != nil {
		return launch.ProjectConfig{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, true, nil
}

func findProjectLaunchJSONPath() (string, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	ceiling := ""
	if top, insideGit := gitWorktreeRootFromCWD(); insideGit {
		ceiling = top
		if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
			cwd = resolved
		}
	}

	dir := cwd
	for ceiling != "" || !isHomeConfigDir(dir) {
		path := filepath.Join(dir, setupConfigPath)
		info, statErr := os.Stat(path)
		if statErr == nil && info.Mode().IsRegular() {
			return path, true, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", false, fmt.Errorf("cannot inspect %s: %w", path, statErr)
		}
		if ceiling != "" && sameCleanPath(dir, ceiling) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false, nil
}
