//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type wakeRepairBootstrapEnv struct {
	values map[string]string
}

var unsetWakeRepairBootstrapEnv = os.Unsetenv

func wakeRepairBootstrapEnvNames() []string {
	names := []string{
		envWakePrivateStopFD,
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	return append(names, wakeRepairBootstrapPlatformEnvNames()...)
}

func captureAndScrubWakeRepairBootstrapEnv() (wakeRepairBootstrapEnv, error) {
	bootstrap := wakeRepairBootstrapEnv{
		values: make(map[string]string),
	}
	names := wakeRepairBootstrapEnvNames()
	for _, name := range names {
		if value, present := os.LookupEnv(name); present {
			bootstrap.values[name] = value
		}
	}

	var scrubErr error
	for _, name := range names {
		if err := unsetWakeRepairBootstrapEnv(name); err != nil {
			scrubErr = errors.Join(
				scrubErr,
				fmt.Errorf("unset private wake repair bootstrap environment %s: %w", name, err),
			)
		}
	}
	if scrubErr != nil {
		return wakeRepairBootstrapEnv{}, scrubErr
	}
	return bootstrap, nil
}

func (bootstrap wakeRepairBootstrapEnv) value(name string) string {
	return strings.TrimSpace(bootstrap.values[name])
}
