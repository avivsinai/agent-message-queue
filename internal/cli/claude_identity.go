package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/claudeidentity"
	"github.com/avivsinai/agent-message-queue/internal/harness"
)

// envClaudeSession is the variable in which Claude Code gives hooks and
// its Bash tool the id of the session they run in.
const envClaudeSession = "CLAUDE_CODE_SESSION_ID"

// envClaudeHostSession is the variable in which an application that hosts
// Claude Code, such as the desktop app, gives it the id of the chat. AMQ
// reads it only in the hook gate; see claudeSessionKey.
const envClaudeHostSession = "CLAUDE_CODE_HOST_SESSION_ID"

// adoptedClaudeBinding is the binding adoptClaudeSessionIdentity adopted
// for this process, or nil.
var adoptedClaudeBinding *claudeidentity.Record

// claudeSessionID returns the id of the Claude Code session this process
// runs in and its Claude config dir, or ok false outside a Claude Code
// session.
func claudeSessionID() (session, configDir string, ok bool) {
	session = strings.TrimSpace(os.Getenv(envClaudeSession))
	home, err := os.UserHomeDir()
	// The variable holds a Claude session id, never a chat id.
	if err != nil || !claudeidentity.ValidSession(session) || claudeidentity.ValidHostSession(session) {
		return "", "", false
	}
	configDir = harness.ClaudeConfigDir(home)
	if !filepath.IsAbs(configDir) {
		return "", "", false
	}
	return session, absPath(filepath.Clean(configDir)), true
}

// claudeSessionKey returns the key AMQ binds this session's identity to,
// and the session's Claude config dir. Under a host application the key is
// the chat's id, so the identity belongs to the chat and stays when /clear
// ends the chat's Claude session and starts another. Claude Code's own
// record of the session decides that, not the environment: a process
// started inside a chat inherits the chat's variables, and Claude Code
// records the chat id only for the session that is the chat. With no chat
// id in that record the key is the Claude session id.
func claudeSessionKey() (key, configDir string, ok bool) {
	session, configDir, ok := claudeSessionID()
	if !ok {
		return "", "", false
	}
	return claudeKeyOf(configDir, session), configDir, true
}

// adoptClaudeSessionIdentity gives an amq command or hook that runs in a
// Claude Code session the identity amq coop join bound to that session. A
// session amq coop exec did not start, such as one of the desktop app, has
// none of the AM_* variables; it does have CLAUDE_CODE_SESSION_ID. Like the
// Codex thread adoption, it acts only when every identity variable is
// absent, validates the recorded identity before any of it becomes visible,
// and treats a binding that cannot be used as a context mismatch, never as
// a reason to fall back to default resolution. A session that never joined,
// or that left, keeps the default resolution.
func adoptClaudeSessionIdentity() error {
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return nil
		}
	}
	session, configDir, ok := claudeSessionKey()
	if !ok {
		return nil
	}
	rec, err := claudeidentity.Lookup(session, configDir)
	if errors.Is(err, claudeidentity.ErrNone) {
		return nil
	}
	if err != nil {
		return ContextMismatchError("this Claude session's AMQ identity cannot be used: %v; run amq coop join --me <handle>, or set AM_ROOT and AM_ME explicitly", err)
	}
	id, err := validateRecordedIdentity(rec.Identity, "this Claude session's")
	if err != nil {
		return err
	}
	if err := adoptIdentity(id, "this Claude session's"); err != nil {
		return err
	}
	rec.Identity = id
	adoptedClaudeBinding = &rec
	return nil
}

// claudeBindingStillHeld reports whether the binding this process adopted
// is still the session's binding. A process that adopted none holds its
// identity from the environment and always passes.
func claudeBindingStillHeld() bool {
	if adoptedClaudeBinding == nil {
		return true
	}
	rec, err := claudeidentity.Lookup(adoptedClaudeBinding.ClaudeSession, adoptedClaudeBinding.ConfigDir)
	return err == nil && rec.Root == adoptedClaudeBinding.Root && rec.Me == adoptedClaudeBinding.Me
}

// managesSessionBinding reports whether args is amq coop join or leave.
// They read and replace the binding themselves, so a damaged binding must
// not stop them at adoption, and an adopted identity must not look like an
// explicit AM_* context to them.
func managesSessionBinding(args []string) bool {
	return len(args) >= 2 && args[0] == "coop" && (args[1] == "join" || args[1] == "leave")
}
