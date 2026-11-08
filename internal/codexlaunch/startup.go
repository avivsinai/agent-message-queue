package codexlaunch

import (
	"bytes"
	"encoding/json"
	"slices"
)

// ConfigResumeCwd reads the effective resume-directory setting without assuming a default.
func ConfigResumeCwd(result []byte) (value string, ok bool) {
	if result == nil {
		return "", false
	}
	var read struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(result, &read) != nil || read.Config == nil {
		return "", false
	}
	tui := bytes.TrimSpace(read.Config["tui"])
	if len(tui) == 0 || string(tui) == "null" {
		return "", true
	}
	var table map[string]json.RawMessage
	if json.Unmarshal(tui, &table) != nil {
		return "", false
	}
	mode := bytes.TrimSpace(table["resume_cwd"])
	if len(mode) == 0 || string(mode) == "null" {
		return "", true
	}
	if json.Unmarshal(mode, &value) != nil {
		return "", false
	}
	return value, true
}

// ConfigUpdateCheckOff requires an explicit effective false value for the update check.
func ConfigUpdateCheckOff(result []byte) bool {
	var read struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(result, &read) != nil {
		return false
	}
	return string(bytes.TrimSpace(read.Config["check_for_update_on_startup"])) == "false"
}

// RequirementsAllowNoUpdateCheck requires readable requirements that do not force the update prompt.
func RequirementsAllowNoUpdateCheck(result []byte) bool {
	var read struct {
		Requirements *struct {
			CheckForUpdateOnStartup *bool `json:"checkForUpdateOnStartup"`
		} `json:"requirements"`
	}
	if json.Unmarshal(result, &read) != nil {
		return false
	}
	return read.Requirements == nil || read.Requirements.CheckForUpdateOnStartup == nil || !*read.Requirements.CheckForUpdateOnStartup
}

// HooksListClear requires a readable directory entry with only trusted or managed hooks.
func HooksListClear(result []byte, dir string) bool {
	type entry struct {
		Cwd   string `json:"cwd"`
		Hooks *[]struct {
			TrustStatus string `json:"trustStatus"`
		} `json:"hooks"`
	}
	var list struct {
		Data []entry `json:"data"`
	}
	if json.Unmarshal(result, &list) != nil {
		return false
	}
	at := slices.IndexFunc(list.Data, func(e entry) bool { return e.Cwd == dir })
	if at < 0 && len(list.Data) == 1 {
		at = 0
	}
	if at < 0 || list.Data[at].Hooks == nil {
		return false
	}
	for _, hook := range *list.Data[at].Hooks {
		if hook.TrustStatus != "trusted" && hook.TrustStatus != "managed" {
			return false
		}
	}
	return true
}
