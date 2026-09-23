package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
)

const (
	harnessHintPrefix = "Installed by amq-acp."
	harnessDocsURL    = "https://github.com/avivsinai/agent-message-queue/blob/main/cmd/amq-acp/README.md"
)

// harnessToken matches a Buzz custom harness id after the amq_ prefix.
var harnessToken = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const installUsage = `amq-acp install writes one Buzz Desktop custom harness for this shell.

Usage:
  amq-acp install --to <handle>
  amq-acp install --remote-target <target>
  amq-acp install --remove --to <handle>
  amq-acp install --remove --remote-target <target>

--to is mailbox mode and sets AMQ_ACP_TO. --remote-target sets
AMQ_ACP_REMOTE_TARGET and does not select a mailbox. Set exactly one.
--remove deletes only the harness file this command wrote.

The file is ~/Library/Application Support/xyz.block.buzz.app/custom_harnesses/amq_<token>.json
with mode 0600. A file this command did not write is left unchanged.
`

type contextInstallError string

func (e contextInstallError) Error() string { return string(e) }

type buzzHarness struct {
	ID                     string            `json:"id"`
	Label                  string            `json:"label"`
	Command                string            `json:"command"`
	Args                   []string          `json:"args"`
	Env                    map[string]string `json:"env"`
	InstallInstructionsURL string            `json:"installInstructionsUrl"`
	InstallHint            string            `json:"installHint"`
}

func runInstall(args []string) int {
	flags := flag.NewFlagSet("amq-acp install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	to := flags.String("to", "", "Mailbox handle that receives prompts")
	remote := flags.String("remote-target", "", "Remote target; sets AMQ_ACP_REMOTE_TARGET")
	remove := flags.Bool("remove", false, "Delete the harness file this command wrote")
	flags.Usage = func() { fmt.Fprint(os.Stderr, installUsage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "amq-acp install: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return exitUsage
	}
	if (*to == "") == (*remote == "") {
		fmt.Fprintln(os.Stderr, "amq-acp install: set exactly one of --to or --remote-target")
		return exitUsage
	}
	token, err := harnessFileToken(*to, *remote)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp install:", err)
		return exitUsage
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp install:", err)
		return exitGeneral
	}
	path := filepath.Join(home, "Library", "Application Support", "xyz.block.buzz.app", "custom_harnesses", "amq_"+token+".json")
	if *remove {
		if err := removeHarness(path); err != nil {
			fmt.Fprintln(os.Stderr, "amq-acp install:", err)
			return exitGeneral
		}
		return 0
	}
	harness, err := buildHarness(*to, *remote, token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp install:", err)
		var contextErr contextInstallError
		if errors.As(err, &contextErr) {
			return exitContextMismatch
		}
		return exitGeneral
	}
	if err := writeHarness(path, harness); err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp install:", err)
		return exitGeneral
	}
	fmt.Printf("In Buzz Desktop: New agent, select %s\n", harness.Label)
	return 0
}

func buildHarness(to, remote, token string) (buzzHarness, error) {
	root, err := absoluteEnv("AM_ROOT")
	if err != nil {
		return buzzHarness{}, err
	}
	me := strings.TrimSpace(os.Getenv("AM_ME"))
	if to != "" {
		if me == "" || !harnessToken.MatchString(me) {
			return buzzHarness{}, contextInstallError("AM_ME is not a handle")
		}
	} else if me != "" && !harnessToken.MatchString(me) {
		return buzzHarness{}, contextInstallError("AM_ME is not a handle")
	}
	command, err := currentExecutable()
	if err != nil {
		return buzzHarness{}, err
	}
	env := map[string]string{
		"AM_ROOT": root,
	}
	if me != "" {
		env["AM_ME"] = me
	}
	base, err := optionalAbsoluteEnv("AM_BASE_ROOT")
	if err != nil {
		return buzzHarness{}, err
	}
	if base != "" {
		env["AM_BASE_ROOT"] = base
	}
	session, err := optionalPlainEnv("AM_SESSION")
	if err != nil {
		return buzzHarness{}, err
	}
	if session != "" {
		env["AM_SESSION"] = session
	}
	label := "AMQ → " + token + " (" + projectName(root, base) + ")"
	hint := harnessHintPrefix + " Prompts go to handle " + to + "."
	if to != "" {
		env["AMQ_ACP_TO"] = to
	} else {
		native, err := remoteNativeSession(root, remote)
		if err != nil {
			return buzzHarness{}, err
		}
		env["AMQ_ACP_REMOTE_TARGET"] = remote
		env["AMQ_ACP_REMOTE_NATIVE_SESSION"] = native
		label = "AMQ remote → " + remote + " (" + projectName(root, base) + ")"
		hint = harnessHintPrefix + " Remote target " + remote + "."
	}
	for _, key := range []string{"AM_ROOT_ID", "AM_BASE_ROOT_ID"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env[key] = value
		}
	}
	return buzzHarness{
		ID:                     "amq_" + token,
		Label:                  label,
		Command:                command,
		Args:                   []string{},
		Env:                    env,
		InstallInstructionsURL: harnessDocsURL,
		InstallHint:            hint,
	}, nil
}

// harnessFileToken is the harness id after amq_. A mailbox handle is used as
// itself. A remote target may contain ':' (claude:98402); that colon becomes
// '_' in the file name, and the raw target stays in the environment.
// remoteNativeSession asks the endpoint which native session target is
// attached to. That id is pinned into the harness. An unreachable endpoint
// or an unshared target both refuse with the same next step.
func remoteNativeSession(root, target string) (string, error) {
	resp, err := ipc.Call(filepath.Join(root, "extensions", "remote"), ipc.Request{
		Native: &ipc.NativeQuery{TargetID: target},
	})
	if err != nil || resp == nil || resp.Error != nil {
		return "", fmt.Errorf("start amq-remote up --root %s first", root)
	}
	var reply ipc.NativeReply
	if json.Unmarshal(resp.Reply, &reply) != nil || reply.NativeSession == "" {
		return "", fmt.Errorf("start amq-remote up --root %s first", root)
	}
	return reply.NativeSession, nil
}

func harnessFileToken(to, remote string) (string, error) {
	if to != "" {
		if !harnessToken.MatchString(to) {
			return "", fmt.Errorf("%q is not a harness id token", to)
		}
		return to, nil
	}
	token := strings.ReplaceAll(remote, ":", "_")
	if !harnessToken.MatchString(token) {
		return "", fmt.Errorf("%q is not a harness id token", remote)
	}
	return token, nil
}

func absoluteEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", contextInstallError(key + " is not set")
	}
	if !filepath.IsAbs(value) {
		return "", contextInstallError(key + " must be absolute")
	}
	return value, nil
}

func optionalAbsoluteEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", nil
	}
	if !filepath.IsAbs(value) {
		return "", contextInstallError(key + " must be absolute")
	}
	return value, nil
}

func optionalPlainEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", nil
	}
	if strings.ContainsAny(value, `/\`) {
		return "", contextInstallError(key + " is not set")
	}
	return value, nil
}

func projectName(root, base string) string {
	dir := root
	if base != "" {
		dir = base
	}
	if filepath.Base(dir) == ".agent-mail" {
		dir = filepath.Dir(dir)
	}
	name := filepath.Base(dir)
	if name == "." || name == string(filepath.Separator) {
		return "amq"
	}
	return name
}

func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("amq-acp path %s is not absolute", resolved)
	}
	// A Homebrew bin/amq-acp symlink and the Cellar binary are the same file.
	// EvalSymlinks would store the versioned Cellar path, which brew cleanup
	// removes. Prefer the PATH entry when it names this executable.
	looked, err := exec.LookPath("amq-acp")
	if err != nil {
		return resolved, nil
	}
	if !filepath.IsAbs(looked) {
		looked, err = filepath.Abs(looked)
		if err != nil {
			return resolved, nil
		}
	}
	lookedInfo, lookedErr := os.Stat(looked)
	exeInfo, exeErr := os.Stat(exe)
	if lookedErr == nil && exeErr == nil && os.SameFile(lookedInfo, exeInfo) {
		return looked, nil
	}
	return resolved, nil
}

func writeHarness(path string, harness buzzHarness) error {
	if raw, err := os.ReadFile(path); err == nil {
		if !wroteHarness(raw) {
			return fmt.Errorf("refusing to overwrite %s; amq-acp did not write it", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.MarshalIndent(harness, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".amq-harness-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func removeHarness(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("harness %s does not exist", path)
	}
	if err != nil {
		return err
	}
	if !wroteHarness(raw) {
		return fmt.Errorf("refusing to remove %s; amq-acp did not write it", path)
	}
	return os.Remove(path)
}

func wroteHarness(raw []byte) bool {
	var harness buzzHarness
	if err := json.Unmarshal(raw, &harness); err != nil {
		return false
	}
	return strings.HasPrefix(harness.InstallHint, harnessHintPrefix)
}
