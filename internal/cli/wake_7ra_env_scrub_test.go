//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"testing"
)

// Test7raWakeChildEnvScrubbed (7ra) proves the amq wake injector child does not
// inherit the full parent shell env. A compromised/buggy injector child must
// not see unrelated parent-shell secrets/tokens.
//
// Failure mode: wakeCommandEnv(os.Environ(), ...) and the attachWakeAttentionFD
// nil-fallback previously passed the full parent env (os.Environ()) to the
// child. A parent shell that exports SECRET_TOKEN=... would leak it into the
// injector child process.
//
// Fix (7ra): scrubWakeChildEnv filters the base env to an allowlist
// (AM_*/AMQ_* + PATH + HOME) before the child is spawned. wakeCommandEnv
// scrubs its base; attachWakeAttentionFD scrubs its nil-fallback.
//
// Mutation RED: scrubWakeChildEnv returns its input unchanged (no filtering) ->
// SECRET_TOKEN survives into the child env -> the assertion fails.
func Test7raWakeChildEnvScrubbed(t *testing.T) {
	// Simulate a parent shell env with AMQ-internal vars, required OS vars, and
	// unrelated secrets that must NOT leak.
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/tmp/fake-home",
		"SECRET_TOKEN=super-secret-value",
		"AWS_SECRET_ACCESS_KEY=leak-me-not",
		"SHLVL=2",
		"PS1=prompt",
		"AM_ROOT=/tmp/amq-root",
		"AM_ME=codex",
		"AMQ_WAKE_OWNER=encoded-owner",
		"AMQ_GLOBAL_ROOT=/tmp/global",
	}
	scrubbed := scrubWakeChildEnv(parent)
	scrubbedMap := make(map[string]string, len(scrubbed))
	for _, kv := range scrubbed {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				scrubbedMap[kv[:i]] = kv[i+1:]
				break
			}
		}
	}

	// Secrets must be dropped.
	for _, leaked := range []string{"SECRET_TOKEN", "AWS_SECRET_ACCESS_KEY", "SHLVL", "PS1"} {
		if _, ok := scrubbedMap[leaked]; ok {
			t.Fatalf("scrubWakeChildEnv leaked non-allowlisted env var %s into the wake child env (7ra): must be dropped", leaked)
		}
	}
	// Required OS vars + AMQ-internal vars must survive.
	for _, keep := range []string{"PATH", "HOME", "AM_ROOT", "AM_ME", "AMQ_WAKE_OWNER", "AMQ_GLOBAL_ROOT"} {
		if _, ok := scrubbedMap[keep]; !ok {
			t.Fatalf("scrubWakeChildEnv dropped required env var %s (7ra): must be retained on the allowlist", keep)
		}
	}

	// wakeCommandEnv must scrub its base too: a SECRET_TOKEN in the parent env
	// must not survive into the built wake child env.
	env, err := wakeCommandEnv(parent, "/tmp/amq-root", nil)
	if err != nil {
		t.Fatalf("wakeCommandEnv: %v", err)
	}
	for _, kv := range env {
		if len(kv) > len("SECRET_TOKEN=") && kv[:len("SECRET_TOKEN=")] == "SECRET_TOKEN=" {
			t.Fatalf("wakeCommandEnv leaked SECRET_TOKEN into the wake child env (7ra): %s", kv)
		}
	}
	// wakeCommandEnv must still set AM_ROOT to the provided root (override).
	for _, kv := range env {
		if len(kv) > len("AM_ROOT=") && kv[:len("AM_ROOT=")] == "AM_ROOT=" {
			if kv != "AM_ROOT=/tmp/amq-root" {
				t.Fatalf("wakeCommandEnv did not set AM_ROOT to the provided root: got %s", kv)
			}
		}
	}
}

// Test7raAttachWakeAttentionFDScrubbedNilEnv (7ra) proves the
// attachWakeAttentionFD nil-fallback scrubs the parent env instead of passing
// os.Environ() verbatim.
func Test7raAttachWakeAttentionFDScrubbedNilEnv(t *testing.T) {
	// Poison the process env with a secret; attachWakeAttentionFD must not
	// propagate it when cmd.Env == nil.
	t.Setenv("SECRET_TOKEN_7RA", "leak-me-not")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/tmp/fake-home")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	cmd := exec.Command("true")
	cmd.Env = nil // force the nil-fallback path
	if err := attachWakeAttentionFD(cmd, w); err != nil {
		t.Fatalf("attachWakeAttentionFD: %v", err)
	}
	for _, kv := range cmd.Env {
		if len(kv) > len("SECRET_TOKEN_7RA=") && kv[:len("SECRET_TOKEN_7RA=")] == "SECRET_TOKEN_7RA=" {
			t.Fatalf("attachWakeAttentionFD nil-fallback leaked parent env var SECRET_TOKEN_7RA into the injector child (7ra): %s", kv)
		}
	}
	// PATH/HOME must survive the scrub.
	havePATH, haveHOME := false, false
	for _, kv := range cmd.Env {
		if kv == "PATH=/usr/bin:/bin" {
			havePATH = true
		}
		if kv == "HOME=/tmp/fake-home" {
			haveHOME = true
		}
	}
	if !havePATH {
		t.Fatalf("attachWakeAttentionFD nil-fallback dropped PATH (7ra): required for exec.LookPath")
	}
	if !haveHOME {
		t.Fatalf("attachWakeAttentionFD nil-fallback dropped HOME (7ra): required for os.UserHomeDir")
	}
}
