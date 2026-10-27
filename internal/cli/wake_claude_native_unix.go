//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

var errClaudeNativeRegistryMismatch = errors.New("native parent registry does not match the bound launch")

const (
	envClaudeNativeLaunch     = "AMQ_CLAUDE_NATIVE_LAUNCH"
	claudeNativeStartupKind   = "claude-native-startup"
	claudeNativeReadyName     = ".wake.claude-native-ready"
	claudeNativeStateName     = ".wake.claude-native"
	claudeNativeMetadataLimit = 64 << 10
)

type claudeNativeLaunch struct {
	SessionID string                `json:"session_id"`
	Cwd       string                `json:"cwd"`
	Home      string                `json:"home"`
	Launch    launch.HolderIdentity `json:"launch"`
}

type claudeNativeReady struct {
	Schema    int                   `json:"schema"`
	SessionID string                `json:"session_id"`
	Launch    launch.HolderIdentity `json:"launch"`
}

type claudeNativeRegistry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Version   string `json:"version"`
	Protocol  int    `json:"peerProtocol"`
	Kind      string `json:"kind"`
	Socket    string `json:"messagingSocketPath"`
}

func prepareClaudeNativeLaunch(ticket *launch.ExecutionTicket) (*claudeNativeLaunch, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("claude-native requires macOS")
	}
	if ticket == nil || ticket.Provider != launch.ClaudeProvider || ticket.Mode != launch.AdapterModeMint ||
		ticket.ProviderVersion != "2.1.311" || ticket.ConversationID == "" || ticket.ConversationID != ticket.LaunchNonce || ticket.Wrapper != nil {
		return nil, fmt.Errorf("claude-native requires a fresh managed Claude 2.1.311 launch without a wrapper")
	}
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return nil, fmt.Errorf("claude-native does not support a custom CLAUDE_CONFIG_DIR")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		return nil, err
	}
	return &claudeNativeLaunch{SessionID: ticket.ConversationID, Cwd: ticket.Cwd, Home: filepath.Join(home, ".claude"), Launch: holder}, nil
}

func readClaudeNativeLaunch() (*claudeNativeLaunch, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("claude-native requires macOS")
	}
	raw := os.Getenv(envClaudeNativeLaunch)
	if raw == "" || len(raw) > 4096 {
		return nil, fmt.Errorf("claude-native requires its private managed launch binding")
	}
	var binding claudeNativeLaunch
	if err := json.Unmarshal([]byte(raw), &binding); err != nil {
		return nil, fmt.Errorf("invalid native launch binding: %w", err)
	}
	if binding.SessionID == "" || !filepath.IsAbs(binding.Cwd) || !filepath.IsAbs(binding.Home) || binding.Launch.PID <= 0 || binding.Launch.ProcessStart == "" {
		return nil, fmt.Errorf("incomplete native launch binding")
	}
	return &binding, nil
}

func readClaudeNativeRegistry(binding claudeNativeLaunch) (claudeNativeRegistry, error) {
	path := filepath.Join(binding.Home, "sessions", strconv.Itoa(binding.Launch.PID)+".json")
	raw, err := fsq.ReadRegularNoFollowBounded(path, claudeNativeMetadataLimit)
	if err != nil {
		return claudeNativeRegistry{}, err
	}
	var reg claudeNativeRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return reg, err
	}
	if reg.PID != binding.Launch.PID || reg.SessionID != binding.SessionID || reg.Cwd != binding.Cwd ||
		reg.Version != "2.1.311" || reg.Protocol != 1 || reg.Kind != "interactive" || !filepath.IsAbs(reg.Socket) {
		return reg, errClaudeNativeRegistryMismatch
	}
	return reg, nil
}

// acknowledgeClaudeNativeStartup accepts only the launched session's own
// startup/prompt hook. Trust records and marker absence do not create a ready signal.
func acknowledgeClaudeNativeStartup(dirfd int, dir *wakeAgentDir, marker wakeDialogMarker, input wakeDialogHookInput) bool {
	if marker.Launch == nil || marker.SessionID == "" || input.emptyInput || !input.ownIdentity ||
		input.SessionID != marker.SessionID || (input.Event != "SessionStart" && input.Event != "UserPromptSubmit") ||
		!launch.RunsUnderProcess(*marker.Launch) {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	binding := claudeNativeLaunch{SessionID: marker.SessionID, Cwd: marker.Cwd, Home: filepath.Join(home, ".claude"), Launch: *marker.Launch}
	if _, err := readClaudeNativeRegistry(binding); err != nil {
		return false
	}
	raw, err := json.Marshal(claudeNativeReady{Schema: 1, SessionID: marker.SessionID, Launch: *marker.Launch})
	if err != nil {
		return false
	}
	return writeWakeRepairMetadataAt(dirfd, dir, claudeNativeReadyName, "native startup acknowledgement", append(raw, '\n'), claudeNativeMetadataLimit) == nil
}

// The launch binding allows the positive hook acknowledgement to survive
// another installed close handler removing the startup marker first.
func acknowledgeClaudeNativeHook(dirfd int, dir *wakeAgentDir, input wakeDialogHookInput) {
	binding, err := readClaudeNativeLaunch()
	if err != nil {
		return
	}
	marker := wakeDialogMarker{SessionID: binding.SessionID, Cwd: binding.Cwd, Launch: &binding.Launch}
	_ = acknowledgeClaudeNativeStartup(dirfd, dir, marker, input)
}
