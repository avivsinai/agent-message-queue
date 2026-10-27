package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A Buzz agent's chat reply exists only if the agent publishes it: buzz-acp
// sends ACP answer text to the owner's activity observer, not to the
// conversation (block/buzz crates/buzz-acp/src/acp.rs:1799, base_prompt.md:71).
// So amq-acp posts every text the owner must read into the triggering
// channel itself, as the managed agent.

// buzzIdentityKeys are the managed agent's credentials that the buzz CLI
// needs. PrepareWorkerEnv captures them before it strips BUZZ_* from the
// environment; they leave this process only as the buzz child's env.
var buzzIdentityKeys = []string{"BUZZ_PRIVATE_KEY", "BUZZ_AUTH_TAG", "BUZZ_RELAY_URL"}

// buzzIdentity holds "KEY=value" entries for buzzIdentityKeys.
var buzzIdentity []string

// envBuzzCLI overrides the buzz binary used to post.
const envBuzzCLI = "AMQ_ACP_BUZZ_CLI"

// defaultPostTimeout bounds one post, including the wait for the CLI's
// output pipes to close (Config.PostTimeout).
const defaultPostTimeout = 30 * time.Second

// postAnswer publishes content into channel and returns the posted event
// id. A variable so tests can record posts without a relay.
var postAnswer = postWithBuzzCLI

// errNoBuzzIdentity means this process runs outside a Buzz managed agent, so
// there is nothing to post as; the ACP text is the only channel.
var errNoBuzzIdentity = errors.New("no Buzz agent identity")

// notSentError marks a post that failed before the buzz CLI started, so
// nothing can have reached the relay and the post may be made again.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// channelRe finds the channel UUID in the Channel line of the prompt's
// <context> block, the reply destination buzz-acp supplies.
var channelRe = regexp.MustCompile(`(?m)^Channel: [^\n]*\(#([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\)`)

// dmScopeRe finds the scope line buzz-acp writes for a DM in the <context>
// block (block/buzz crates/buzz-acp/src/queue.rs).
var dmScopeRe = regexp.MustCompile(`(?m)^Scope: dm\r?$`)

// buzzDM reports whether a buzz-acp prompt came from a DM with a channel.
func buzzDM(prompt string) bool {
	start := strings.Index(prompt, "<context>")
	end := strings.Index(prompt, "</context>")
	return start >= 0 && end > start && dmScopeRe.MatchString(prompt[start:end]) && buzzChannel(prompt) != ""
}

// buzzChannel returns the reply channel named by a buzz-acp prompt, or "".
func buzzChannel(prompt string) string {
	start := strings.Index(prompt, "<context>")
	end := strings.Index(prompt, "</context>")
	if start < 0 || end < start {
		return ""
	}
	if m := channelRe.FindStringSubmatch(prompt[start:end]); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// uuidRe is a channel UUID as buzz-acp writes it.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// hostScopes are the values buzz-acp writes on the Scope and Session scope
// lines of its <context> section (queue.rs format_context_hints).
var hostScopes = []struct {
	prefix  string
	allowed []string
}{
	{"Scope: ", []string{"dm", "thread", "channel"}},
	{"Session scope: ", []string{"dm conversation", "thread", "channel"}},
}

// hostContext returns the channel UUID of the prompt's host context, the
// <context> section buzz-acp emits as its own content block, or "" when the
// prompt has no usable one. It examines the blocks one by one, never the
// joined text, so a Channel line inside conversation context or an event
// cannot become the route. The section must be the only text block that
// starts with the line <context> and ends with the line </context>, and
// must hold exactly one column-0 Channel line and at most one of each scope
// line, every one well formed. buzz-acp indents and escapes descriptions, so
// only a channel name with a line break can add a structural line; such a
// prompt is refused, never resolved. The UUID is the one at the end of the
// Channel line, so a name that contains "(#...)" cannot move it.
func hostContext(blocks []contentBlock) string {
	var section []string
	found := false
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		lines := strings.Split(b.Text, "\n")
		if len(lines) < 2 || lines[0] != "<context>" || lines[len(lines)-1] != "</context>" {
			continue
		}
		if found {
			return ""
		}
		found, section = true, lines[1:len(lines)-1]
	}
	channel, channels := "", 0
	seen := make([]bool, len(hostScopes))
	for _, line := range section {
		if strings.HasPrefix(line, "Channel:") {
			channels++
			if channel = hostChannel(line); channel == "" {
				return ""
			}
			continue
		}
		for i, scope := range hostScopes {
			if !strings.HasPrefix(line, strings.TrimSuffix(scope.prefix, " ")) {
				continue
			}
			value, ok := strings.CutPrefix(line, scope.prefix)
			if seen[i] || !ok || !slices.Contains(scope.allowed, value) {
				return ""
			}
			seen[i] = true
		}
	}
	if channels != 1 {
		return ""
	}
	return channel
}

// hostChannel parses one Channel line: "Channel: <name> (#<uuid>)", or
// "Channel: <uuid>" when buzz-acp has no channel info.
func hostChannel(line string) string {
	rest, ok := strings.CutPrefix(line, "Channel: ")
	if !ok {
		return ""
	}
	if uuidRe.MatchString(rest) {
		return strings.ToLower(rest)
	}
	open := strings.LastIndex(rest, " (#")
	if open < 0 || !strings.HasSuffix(rest, ")") {
		return ""
	}
	if id := rest[open+3 : len(rest)-1]; uuidRe.MatchString(id) {
		return strings.ToLower(id)
	}
	return ""
}

// buzzSelf is the managed agent's own public key (64 lowercase hex), or ""
// when the secret is absent or unreadable. The agent archives and unarchives
// itself under it (lifecycle.go).
var buzzSelf string

// captureBuzzIdentity records the credentials the buzz CLI needs, the agent's
// own public key, and the agent's owner (approval.go).
func captureBuzzIdentity() {
	buzzIdentity = nil
	for _, key := range buzzIdentityKeys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			buzzIdentity = append(buzzIdentity, key+"="+v)
		}
	}
	buzzSelf = ""
	if secret := strings.TrimSpace(os.Getenv("BUZZ_PRIVATE_KEY")); secret != "" {
		if pub, err := agentPubKey(secret); err == nil {
			buzzSelf = pub
		}
	}
	buzzOwner = agentOwner(os.Getenv("BUZZ_AUTH_TAG"), os.Getenv("BUZZ_PRIVATE_KEY"), os.Getenv("BUZZ_ACP_AGENT_OWNER"))
	buzzRelay = agentRelay(os.Getenv("BUZZ_RELAY_URL"), os.Getenv("BUZZ_PRIVATE_KEY"), os.Getenv("BUZZ_AUTH_TAG"))
	buzzRelayURL, buzzRelayKey = strings.TrimSpace(os.Getenv("BUZZ_RELAY_URL")), relayKey(os.Getenv("BUZZ_RELAY_URL"))
}

// postWithBuzzCLI runs `buzz messages send --channel <channel> --content -`
// with only the agent identity and a minimal environment, and returns the
// event_id the CLI prints.
func postWithBuzzCLI(channel, content string, budget time.Duration) (string, error) {
	out, err := runBuzzCLI(budget, strings.NewReader(content), "messages", "send", "--channel", channel, "--content", "-")
	if err != nil {
		return "", err
	}
	var sent struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(out, &sent); err != nil || sent.EventID == "" {
		return "", fmt.Errorf("buzz messages send: no event_id in its output")
	}
	return sent.EventID, nil
}

// runBuzzCLI runs the buzz CLI with only the agent identity and a minimal
// environment and returns its stdout.
func runBuzzCLI(budget time.Duration, stdin io.Reader, args ...string) ([]byte, error) {
	return runBuzzCLIContext(context.Background(), budget, stdin, args...)
}

// runBuzzCLIContext is runBuzzCLI that also stops when parent ends. A run
// that never started is a notSentError: it cannot have reached the relay.
func runBuzzCLIContext(parent context.Context, budget time.Duration, stdin io.Reader, args ...string) ([]byte, error) {
	if len(buzzIdentity) == 0 {
		return nil, errNoBuzzIdentity
	}
	bin, err := buzzCLI()
	if err != nil {
		var notSent *notSentError
		if !errors.As(err, &notSent) {
			err = &notSentError{err}
		}
		return nil, err
	}
	// The budget covers the run and the pipe drain: a CLI whose child keeps
	// stderr open must not hold a status post (and its event post lock)
	// past the budget. A CLI that keeps sending after its budget is outside
	// the status/final ordering (README).
	waitDelay := budget / 10
	ctx, cancel := context.WithTimeout(parent, budget-waitDelay)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, buzzIdentity...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, &notSentError{fmt.Errorf("buzz %s: %w", strings.Join(args[:2], " "), err)}
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			code = exit.ExitCode()
		}
		return nil, &buzzExitError{code: code, err: fmt.Errorf("buzz %s: %v: %s", strings.Join(args[:2], " "), err, msg)}
	}
	return stdout.Bytes(), nil
}

// buzzExitError is a buzz CLI run that started and failed. code is the
// CLI's exit status: 1 bad input, 2 relay or network, 3 auth, 4 other, 5
// write conflict; -1 when the run was cut off (a timeout or a signal).
type buzzExitError struct {
	code int
	err  error
}

func (e *buzzExitError) Error() string { return e.err.Error() }
func (e *buzzExitError) Unwrap() error { return e.err }

// buzzCLI finds the buzz binary that receives the managed agent's key:
// AMQ_ACP_BUZZ_CLI, else the CLI inside Buzz.app under /Applications or the
// user's Applications.
// There is no PATH fallback: an unrelated buzz on PATH must never receive
// BUZZ_PRIVATE_KEY.
func buzzCLI() (string, error) {
	if p := strings.TrimSpace(os.Getenv(envBuzzCLI)); p != "" {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	return buzzCLIFrom("/Applications", home)
}

// buzzCLIFrom selects the first usable Buzz.app CLI: systemApps first, then
// home/Applications. A home that is not absolute yields no candidate.
func buzzCLIFrom(systemApps, home string) (string, error) {
	dirs := []string{systemApps}
	if filepath.IsAbs(home) {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	for _, d := range dirs {
		if p := bundledBuzz(filepath.Join(d, "Buzz.app")); p != "" {
			return p, nil
		}
	}
	return "", &notSentError{errors.New("buzz CLI not found in Buzz.app; set " + envBuzzCLI + " to the buzz executable")}
}

// bundledBuzz returns the CLI inside one Buzz.app, or "" unless the whole path
// from the filesystem root down contains no symlink (so an ancestor such as a
// symlinked home or Applications cannot redirect the key to another tree), the
// CLI is a regular file, and this process can execute it. The installation and
// its parent directories are trusted against concurrent replacement: pathname
// validation is not atomic with exec.
func bundledBuzz(app string) string {
	path := filepath.Join(app, "Contents", "MacOS", "buzz")
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != filepath.Clean(path) {
		return ""
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || !canExecute(path) {
		return ""
	}
	return path
}

// publish posts text for the owner when the prompt came from Buzz. It
// reports the outcome for the result meta: "posted", "" when there is no
// Buzz channel or identity, or the error.
func (s *Server) publish(channel, text string) string {
	result, _ := s.post(channel, text)
	return result
}

// Outbound states of an event's final post, each a marker beside the final
// marker. accepted, uncertain and unsent are created once a post returns; a
// retry renames unsent to sending before it posts. A final marker with no
// unsent marker is a post in flight, interrupted, or made by an older
// build: uncertain, never posted again.
const (
	finalAccepted  = "accepted"
	finalUncertain = "uncertain"
	finalUnsent    = "unsent"
	finalSending   = "sending"
)

// publishFinal is publish for an event's final text: it also records what
// the post proves. Only a post that failed before the CLI started is
// unsent, and the late-reply sweep may make it again (resendFinal).
func (s *Server) publishFinal(eventID, channel, text string) string {
	result, err := s.post(channel, text)
	if eventID == "" || channel == "" || strings.TrimSpace(text) == "" {
		return result
	}
	return s.recordFinal(eventID, result, err)
}

// recordFinal records what a final post proves and returns its result.
func (s *Server) recordFinal(eventID, result string, err error) string {
	if errors.Is(err, errNoBuzzIdentity) {
		return result
	}
	state := finalAccepted
	var notSent *notSentError
	switch {
	case errors.As(err, &notSent):
		state = finalUnsent
	case err != nil:
		state = finalUncertain
	}
	if _, err := createExclusive(s.finalStatePath(eventID, state), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n")); err != nil {
		return result + "; record post state: " + err.Error()
	}
	return result
}

// post is publish that also returns the post's error.
func (s *Server) post(channel, text string) (string, error) {
	if channel == "" || strings.TrimSpace(text) == "" {
		return "", nil
	}
	_, err := postAnswer(channel, text, s.cfg.PostTimeout)
	switch {
	case err == nil:
		return "posted", nil
	case errors.Is(err, errNoBuzzIdentity):
		return "", err
	default:
		return "error: " + err.Error(), err
	}
}

func (s *Server) finalStatePath(eventID, state string) string {
	return filepath.Join(s.eventDir(eventID), eventID+".posted."+postFinal+"."+state)
}

// finalUnsent reports whether the event's final answer is a reply that was
// definitely never posted: unsent, and neither accepted nor uncertain.
func (s *Server) finalUnsent(eventID string, out mailboxOutcome) bool {
	if out.ReplyID == "" || out.damaged != nil {
		return false
	}
	for _, state := range []string{finalAccepted, finalUncertain} {
		if _, err := os.Lstat(s.finalStatePath(eventID, state)); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	_, err := os.Lstat(s.finalStatePath(eventID, finalUnsent))
	return err == nil
}
