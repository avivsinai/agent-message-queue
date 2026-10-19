package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	keepaliveSettingsFileName = "settings.json"
	keepaliveSettingsSchemaV1 = 1
	keepaliveSettingsMaxBytes = 64 << 10

	keepaliveSettingsStatusOK      = "ok"
	keepaliveSettingsStatusAbsent  = "absent"
	keepaliveSettingsStatusRefused = "refused"
)

// keepaliveSettingsRead is one read of the keepalive settings file, decoded.
// The file is {"schema":1,"settings":{"self_upgrade":false}}; a missing file
// or key has the default. raw and err identify the observation, so a
// supervisor reports a change once.
type keepaliveSettingsRead struct {
	exists      bool
	raw         []byte
	err         error
	selfUpgrade bool
	// selfUpgradeSet is true when the file sets self_upgrade.
	selfUpgradeSet bool
}

// keepaliveSettingsPath is the settings file in the keepalive home, beside
// the registry and the self-upgrade state.
func keepaliveSettingsPath(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), keepaliveSettingsFileName)
}

func (read keepaliveSettingsRead) sameObservation(other keepaliveSettingsRead) bool {
	return read.exists == other.exists &&
		bytes.Equal(read.raw, other.raw) &&
		fmt.Sprint(read.err) == fmt.Sprint(other.err)
}

// readKeepaliveSettings reads and decodes the file strictly: a symlink, a
// non-regular file, a newer schema, an unknown key, a wrong type, or trailing
// data is refused.
func readKeepaliveSettings(path string) keepaliveSettingsRead {
	read := keepaliveSettingsRead{selfUpgrade: true}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return read
	}
	read.exists = true
	if err != nil {
		read.err = err
		return read
	}
	if !info.Mode().IsRegular() {
		read.err = fmt.Errorf("%s is not a regular file", path)
		return read
	}
	if info.Size() > keepaliveSettingsMaxBytes {
		read.err = fmt.Errorf("%s is larger than %d bytes", path, keepaliveSettingsMaxBytes)
		return read
	}
	read.raw, read.err = os.ReadFile(path)
	if read.err != nil {
		return read
	}
	read.err = decodeKeepaliveSettings(read.raw, &read)
	return read
}

func decodeKeepaliveSettings(raw []byte, read *keepaliveSettingsRead) error {
	var envelope struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode keepalive settings: %w", err)
	}
	if envelope.Schema == nil {
		return errors.New("decode keepalive settings: schema is missing")
	}
	if *envelope.Schema != keepaliveSettingsSchemaV1 {
		return fmt.Errorf("keepalive settings schema %d is not supported (want %d)", *envelope.Schema, keepaliveSettingsSchemaV1)
	}
	var file struct {
		Schema   int                        `json:"schema"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return fmt.Errorf("decode keepalive settings: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("decode keepalive settings: trailing data after the document")
	}
	for key, value := range file.Settings {
		if key != "self_upgrade" {
			return fmt.Errorf("decode keepalive settings: unknown key %q (valid: self_upgrade)", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("decode keepalive setting self_upgrade: value is null")
		}
		if err := json.Unmarshal(value, &read.selfUpgrade); err != nil {
			return fmt.Errorf("decode keepalive setting self_upgrade: %w", err)
		}
		read.selfUpgradeSet = true
	}
	return nil
}

// reloadSettings re-reads the settings file once per supervise pass and
// returns a line to log, or "" when the file is unchanged. Only
// self_upgrade=false takes effect live; true cannot enable a supervisor
// started with --no-self-upgrade, because enabling needs the startup
// baseline. A refused file keeps the last good value.
func (controller *selfUpgradeController) reloadSettings() string {
	if controller == nil || controller.settingsPath == "" {
		return ""
	}
	read := readKeepaliveSettings(controller.settingsPath)
	if controller.settingsSeen && read.sameObservation(controller.settingsObserved) {
		return ""
	}
	controller.settingsSeen = true
	controller.settingsObserved = read
	if read.err != nil {
		return fmt.Sprintf(
			"amq-keepalive settings %s refused: %v; keeping self_upgrade=%t",
			controller.settingsPath, read.err, !controller.disabledBySettings,
		)
	}
	wasDisabled := controller.disabledBySettings
	controller.disabledBySettings = !read.selfUpgrade
	switch {
	case !read.selfUpgrade && !wasDisabled:
		return fmt.Sprintf("amq-keepalive self-upgrade disabled by self_upgrade=false in %s", controller.settingsPath)
	case read.selfUpgrade && read.selfUpgradeSet && !controller.enabled:
		return fmt.Sprintf(
			"amq-keepalive self-upgrade stays %s; self_upgrade=true in %s needs a restart to enable",
			controller.reason, controller.settingsPath,
		)
	case read.selfUpgrade && wasDisabled:
		return fmt.Sprintf("amq-keepalive self-upgrade is no longer disabled by %s", controller.settingsPath)
	}
	return ""
}
