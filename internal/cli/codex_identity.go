package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// identityEnvKeys are the variables that make up an AMQ participant
// identity. Presence of any one, even empty, is an explicit context.
var identityEnvKeys = []string{envRoot, envBaseRoot, envSession, envMe, envRootID, envBaseRootID}

// adoptCodexThreadIdentity gives an amq command run inside a Codex thread the
// identity amq coop exec recorded for that thread (agent-message-queue-611.61).
// On Codex 0.160 tool commands run in the shared daemon's environment and see
// none of the AM_* variables coop exec set, only CODEX_THREAD_ID. It acts only
// when every identity variable is absent; the recorded identity is fully
// validated before any of it becomes visible, and then the usual pin checks
// run on it. A thread AMQ never recorded keeps today's behavior. A recorded
// but unusable identity (damaged, foreign CODEX_HOME, replaced by a later
// coop exec, or a root that changed) is a context mismatch for every command,
// never a fallback to default resolution; setting the identity variables
// explicitly is the way out.
func adoptCodexThreadIdentity() error {
	thread, ok := os.LookupEnv("CODEX_THREAD_ID")
	if !ok || strings.TrimSpace(thread) == "" {
		return nil
	}
	for _, key := range identityEnvKeys {
		if _, present := os.LookupEnv(key); present {
			return nil
		}
	}
	codexHome, err := codexHomeDir()
	if err != nil {
		return ContextMismatchError("cannot resolve CODEX_HOME for this Codex thread's AMQ identity: %v", err)
	}
	rec, err := codexidentity.Lookup(strings.TrimSpace(thread), absPath(filepath.Clean(codexHome)))
	if errors.Is(err, codexidentity.ErrNone) {
		return nil
	}
	if err != nil {
		return ContextMismatchError("this Codex thread's AMQ identity cannot be used: %v; start the session again with amq coop exec, or set AM_ROOT and AM_ME explicitly", err)
	}
	if err := validateRecordedIdentity(rec); err != nil {
		return err
	}
	values := map[string]string{
		envRoot: rec.Root, envBaseRoot: rec.BaseRoot, envSession: rec.Session,
		envMe: rec.Me, envRootID: rec.RootID, envBaseRootID: rec.BaseRootID,
	}
	for _, key := range identityEnvKeys {
		if err := os.Setenv(key, values[key]); err != nil {
			for _, k := range identityEnvKeys {
				_ = os.Unsetenv(k)
			}
			return ContextMismatchError("cannot adopt this Codex thread's AMQ identity: %v", err)
		}
	}
	return nil
}

// validateRecordedIdentity applies the checks an inherited pin gets, before
// the identity is published: a valid handle and session, absolute roots in
// the right relation, well-formed tokens, and trees that are still the ones
// coop exec pinned.
func validateRecordedIdentity(rec codexidentity.Record) error {
	if handle, err := normalizeHandle(rec.Me); err != nil || handle != rec.Me {
		return ContextMismatchError("this Codex thread's recorded AMQ handle %q is invalid", rec.Me)
	}
	want := rec.BaseRoot
	if rec.Session != "" {
		if err := validateSessionName(rec.Session); err != nil {
			return ContextMismatchError("this Codex thread's recorded AMQ session %q is invalid", rec.Session)
		}
		want = filepath.Join(rec.BaseRoot, rec.Session)
	}
	if filepath.Clean(rec.Root) != filepath.Clean(want) {
		return ContextMismatchError("this Codex thread's recorded AMQ root %s is not %s", rec.Root, want)
	}
	if !validTreeIdentityToken(rec.RootID) || !validTreeIdentityToken(rec.BaseRootID) {
		return ContextMismatchError("this Codex thread's recorded AMQ identity tokens are malformed")
	}
	return verifyRootUnderBase(rec.BaseRoot, rec.BaseRootID, rec.Session, rec.Root, rec.RootID)
}

// codexThreadIdentityRecorder returns the function coop exec calls once, for
// the Codex thread the TUI will resume: it records the identity in env for
// that thread. Without complete root identity tokens it returns nil, so no
// thread is handed to a TUI with an identity AMQ could not record.
func codexThreadIdentityRecorder(env []string) func(thread string) error {
	get := func(key string) string {
		prefix := key + "="
		for _, entry := range env {
			if v, ok := strings.CutPrefix(entry, prefix); ok {
				return v
			}
		}
		return ""
	}
	rec := codexidentity.Record{
		Root: get(envRoot), BaseRoot: get(envBaseRoot), Session: get(envSession),
		Me: get(envMe), RootID: get(envRootID), BaseRootID: get(envBaseRootID),
	}
	if rec.Root == "" || rec.Me == "" || rec.RootID == "" || rec.BaseRootID == "" {
		return nil
	}
	return func(thread string) error {
		codexHome, err := codexHomeDir()
		if err != nil {
			return err
		}
		r := rec
		r.Thread = thread
		r.CodexHome = absPath(filepath.Clean(codexHome))
		return codexidentity.Publish(r)
	}
}

// recordResumedCodexThread records the identity for a Codex thread the user
// resumes by id: the one AMQ named and created, or one the user resumes
// (codex resume <id>, as amq session resume runs it). Its tool commands run
// on the daemon. If the identity cannot be recorded the launch stops: a
// thread is never resumed without it.
func recordResumedCodexThread(binaryPath string, args []string, record func(thread string) error) error {
	if launch.ProviderForExecutable(binaryPath) != launch.CodexProvider ||
		len(args) < 2 || args[0] != "resume" || !codexidentity.ValidThread(args[1]) {
		return nil
	}
	if record == nil {
		return ContextMismatchError("AMQ cannot record this session's identity for Codex thread %s", args[1])
	}
	if err := record(args[1]); err != nil {
		return ContextMismatchError("AMQ could not record this session's identity for Codex thread %s: %v", args[1], err)
	}
	return nil
}
