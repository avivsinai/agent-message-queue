package board

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// ClaudeListBinding is an explicit project-local grant to acquire one list.
// Revision prevents an acquisition from using a removed or replaced grant.
type ClaudeListBinding struct {
	Home     string `json:"home"`
	List     string `json:"list"`
	Revision string `json:"revision"`
}

var nativeName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func ClaudeListKey(home, list string) string {
	b, _ := json.Marshal([]string{home, list})
	return string(b)
}
func CanonicalClaudeList(home, list string) (string, string, error) {
	if !nativeName.MatchString(list) || len(list) > 256 {
		return "", "", errors.New("list ID must use the native sanitized filename alphabet")
	}
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", "", err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", "", err
	}
	return real, ClaudeListKey(real, list), nil
}
func nativeAlias(source string, item Item) (string, bool) {
	if item.Namespace == "" || item.Ref.ID == "" || strings.TrimSpace(item.Title) == "" || hasControl(item.Namespace+item.Ref.ID) {
		return "", false
	}
	switch source {
	case "beads":
		if item.Ref.Kind != RefBead || !beadShapeRe.MatchString(item.Ref.ID) {
			return "", false
		}
	case "codex-goals":
		if item.Ref.Kind != "codex-goal" || !ValidHarnessID(item.Ref.ID) || !ValidHarnessID(item.Container) {
			return "", false
		}
	case "claude-tasks":
		if item.Ref.Kind != "claude-task" || !nativeName.MatchString(item.Ref.ID) {
			return "", false
		}
	default:
		return "", false
	}
	return source + ":" + item.Namespace + ":" + item.Ref.ID, true
}
