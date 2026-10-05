package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A Buzz agent's chat reply exists only if the agent publishes it: buzz-acp
// sends ACP answer text to the owner's activity observer, not to the
// conversation (block/buzz crates/buzz-acp/src/acp.rs:1799, base_prompt.md:71).
// So amq-acp posts every text the owner must read into the triggering
// channel itself, as the managed agent (bead agent-message-queue-611.38).

// buzzIdentityKeys are the managed agent's credentials that the buzz CLI
// needs. PrepareWorkerEnv captures them before it strips BUZZ_* from the
// environment; they leave this process only as the buzz child's env.
var buzzIdentityKeys = []string{"BUZZ_PRIVATE_KEY", "BUZZ_AUTH_TAG", "BUZZ_RELAY_URL"}

// buzzIdentity holds "KEY=value" entries for buzzIdentityKeys.
var buzzIdentity []string

// envBuzzCLI overrides the buzz binary used to post.
const envBuzzCLI = "AMQ_ACP_BUZZ_CLI"

// postTimeout bounds one post.
const postTimeout = 30 * time.Second

// postAnswer publishes content into channel. A variable so tests can record
// posts without a relay.
var postAnswer = postWithBuzzCLI

// errNoBuzzIdentity means this process runs outside a Buzz managed agent, so
// there is nothing to post as; the ACP text is the only channel.
var errNoBuzzIdentity = errors.New("no Buzz agent identity")

// channelRe finds the channel UUID in the Channel line of the prompt's
// <context> block, the reply destination buzz-acp supplies.
var channelRe = regexp.MustCompile(`(?m)^Channel: [^\n]*\(#([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\)`)

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

// captureBuzzIdentity records the credentials the buzz CLI needs.
func captureBuzzIdentity() {
	buzzIdentity = nil
	for _, key := range buzzIdentityKeys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			buzzIdentity = append(buzzIdentity, key+"="+v)
		}
	}
}

// postWithBuzzCLI runs `buzz messages send --channel <channel> --content -`
// with only the agent identity and a minimal environment.
func postWithBuzzCLI(channel, content string) error {
	if len(buzzIdentity) == 0 {
		return errNoBuzzIdentity
	}
	bin, err := buzzCLI()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "messages", "send", "--channel", channel, "--content", "-")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, buzzIdentity...)
	cmd.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("buzz messages send: %v: %s", err, msg)
	}
	return nil
}

// systemApplications is the machine-wide Applications directory.
var systemApplications = "/Applications"

// buzzCLI finds the buzz binary that receives the owner key: AMQ_ACP_BUZZ_CLI,
// else the CLI inside Buzz.app under the system Applications directory or an
// absolute home's. There is no PATH fallback: an unrelated buzz on PATH must
// never receive BUZZ_PRIVATE_KEY (agent-message-queue-fa4).
func buzzCLI() (string, error) {
	if p := strings.TrimSpace(os.Getenv(envBuzzCLI)); p != "" {
		return p, nil
	}
	dirs := []string{systemApplications}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	for _, d := range dirs {
		if p := bundledBuzz(filepath.Join(d, "Buzz.app")); p != "" {
			return p, nil
		}
	}
	return "", errors.New("buzz CLI not found in Buzz.app; set " + envBuzzCLI + " to the buzz executable")
}

// bundledBuzz returns the CLI inside one Buzz.app, or "" unless every
// component from the bundle root down is a real directory (no symlink) and
// the CLI is a regular file the user can execute.
func bundledBuzz(app string) string {
	path := app
	for _, part := range []string{"", "Contents", "MacOS", "buzz"} {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return ""
		}
		if part == "buzz" {
			if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o100 == 0 {
				return ""
			}
		} else if !fi.IsDir() {
			return ""
		}
	}
	return path
}

// publish posts text for the owner when the prompt came from Buzz. It
// reports the outcome for the result meta: "posted", "" when there is no
// Buzz channel or identity, or the error.
func publish(channel, text string) string {
	if channel == "" || strings.TrimSpace(text) == "" {
		return ""
	}
	err := postAnswer(channel, text)
	switch {
	case err == nil:
		return "posted"
	case errors.Is(err, errNoBuzzIdentity):
		return ""
	default:
		return "error: " + err.Error()
	}
}
