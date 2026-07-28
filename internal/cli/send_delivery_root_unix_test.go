//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runBlockedBodyCommandAfterMutation(
	t *testing.T,
	name string,
	command func(bodyPath string) error,
	mutate func(),
) error {
	t.Helper()
	bodyFIFO := filepath.Join(secureTempDirForTest(t), name+".fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	t.Cleanup(func() {
		reader, err := os.OpenFile(bodyFIFO, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = reader.Close()
		}
	})
	errCh := make(chan error, 1)
	go func() {
		errCh <- command(bodyFIFO)
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatalf("open FIFO writer: %v", result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("command returned before body read: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("command did not reach body read")
	}
	mutate()
	if _, err := writer.Write([]byte("release after authorized state changed")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("command did not return after body release")
		return nil
	}
}

func TestSendRejectsSymlinkSwapAfterGuard(t *testing.T) {
	parent := secureTempDirForTest(t)
	authorizedAlias := filepath.Join(parent, "authorized")
	authorizedMoved := filepath.Join(parent, "authorized-moved")
	outside := filepath.Join(parent, "outside")
	for _, root := range []string{authorizedAlias, outside} {
		for _, agent := range []string{"alice", "bob"} {
			if err := fsq.EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs(%s,%s): %v", root, agent, err)
			}
		}
		configureSendTestRoot(t, root, "alice", "bob")
	}
	clearDeliveryRootTestEnv(t)

	bodyFIFO := filepath.Join(parent, "body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	t.Cleanup(func() {
		// Release the blocked writer if runSend returns before opening the FIFO.
		reader, err := os.OpenFile(bodyFIFO, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = reader.Close()
		}
	})
	errCh := make(chan error, 1)
	go func() {
		errCh <- runSend([]string{
			"--root", authorizedAlias,
			"--me", "alice",
			"--to", "bob",
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatalf("open FIFO writer: %v", result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("send returned before reading body: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach post-authorization body read")
	}
	if err := os.Rename(authorizedAlias, authorizedMoved); err != nil {
		t.Fatalf("move authorized root: %v", err)
	}
	if err := os.Symlink(outside, authorizedAlias); err != nil {
		t.Fatalf("replace authorized alias: %v", err)
	}
	if _, err := writer.Write([]byte("after authorization")); err != nil {
		t.Fatalf("write FIFO body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close FIFO writer: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
			t.Fatalf("send error = %v, want post-authorization root-change refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return after body was released")
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(outside, "bob")); err != nil {
		t.Fatalf("ReadDir outside inbox: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("escaped delivery wrote %d message(s) outside authorized root", len(entries))
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(authorizedMoved, "bob")); err != nil {
		t.Fatalf("ReadDir original inbox: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("refused send wrote %d message(s) into moved authorized root", len(entries))
	}
}

func TestSendStrictConfigReplacementBeforeRepairDoesNotCreateOrDeliver(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	missing := fsq.AgentDLQCur(root, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "meta", "config.json")
	bodyFIFO := filepath.Join(secureTempDirForTest(t), "strict-body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatal(err)
	}

	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runSend([]string{
			"--root", root,
			"--me", "alice",
			"--to", "bob",
			"--strict",
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("send returned before body read: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach body read after strict validation")
	}
	original := configPath + ".original"
	if err := os.Rename(configPath, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["alice"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("must not deliver")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "config") {
			t.Fatalf("send error = %v, want retained-config refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("strict send repaired after roster removal: %v", err)
	}
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "bob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("strict send delivered after roster removal: %#v", entries)
	}
}

func TestCrossProjectSendReportsRepairWhenMailboxBecomesIncompleteAtDelivery(t *testing.T) {
	clearDeliveryRootTestEnv(t)
	srcProjectDir := secureTempDirForTest(t)
	srcRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
	peerBase := filepath.Join(secureTempDirForTest(t), "peer base")
	peerRoot := filepath.Join(peerBase, "collab")
	if err := fsq.EnsureAgentDirs(srcRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, peerBase, "bob")
	rcData, err := json.Marshal(map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers":   map[string]string{"peer": peerBase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcProjectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}
	bodyFIFO := filepath.Join(secureTempDirForTest(t), "peer-body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatal(err)
	}

	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runSend([]string{
			"--root", srcRoot,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("send returned before body read: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach body read after peer validation")
	}
	if err := config.WriteConfig(filepath.Join(peerBase, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"carol"},
	}, true); err != nil {
		t.Fatal(err)
	}
	missing := fsq.AgentDLQCur(peerRoot, "bob")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("must not deliver")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case sendErr := <-errCh:
		if sendErr == nil {
			t.Fatal("send succeeded after peer mailbox became incomplete")
		}
		for _, want := range []string{"incomplete", "missing:dlq/cur", `add "bob"`, "peer config", "--root", "--base-root", "--fix-mailboxes"} {
			if !strings.Contains(sendErr.Error(), want) {
				t.Fatalf("delivery-time peer error missing %q: %v", want, sendErr)
			}
		}
		if err := config.WriteConfig(filepath.Join(peerBase, "meta", "config.json"), config.Config{
			Version: 1,
			Agents:  []string{"bob"},
		}, true); err != nil {
			t.Fatal(err)
		}
		executeAdvertisedAMQCommand(t, advertisedPeerRepairCommand(t, sendErr))
		if err := fsq.ValidateExistingMailboxLayout(openDeliveryRootForCLITest(t, peerRoot), "bob"); err != nil {
			t.Fatalf("delivery-time advertised remedy left mailbox incomplete: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return")
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(peerRoot, "bob")); err != nil || len(entries) != 0 {
		t.Fatalf("incomplete peer received message: entries=%#v err=%v", entries, err)
	}
}

func TestCrossProjectSendRevalidatesRosterAfterBlockingBodyRead(t *testing.T) {
	for _, strict := range []bool{true, false} {
		t.Run(map[bool]string{true: "strict_refuses", false: "non_strict_warns"}[strict], func(t *testing.T) {
			clearDeliveryRootTestEnv(t)
			srcProjectDir := secureTempDirForTest(t)
			srcRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
			peerBase := filepath.Join(secureTempDirForTest(t), "peer")
			peerRoot := filepath.Join(peerBase, "collab")
			if err := fsq.EnsureAgentDirs(srcRoot, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
				t.Fatal(err)
			}
			configureSendTestRoot(t, peerBase, "bob")
			rcData, err := json.Marshal(map[string]any{
				"root":    ".agent-mail",
				"project": "source",
				"peers":   map[string]string{"peer": peerBase},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(srcProjectDir, ".amqrc"), rcData, 0o600); err != nil {
				t.Fatal(err)
			}

			bodyFIFO := filepath.Join(secureTempDirForTest(t), "peer-roster-body.fifo")
			if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
				t.Fatal(err)
			}
			type openResult struct {
				file *os.File
				err  error
			}
			writerCh := make(chan openResult, 1)
			go func() {
				file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
				writerCh <- openResult{file: file, err: err}
			}()

			oldStderr := os.Stderr
			stderrReader, stderrWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stderr = stderrWriter
			t.Cleanup(func() { os.Stderr = oldStderr })

			args := []string{
				"--root", srcRoot,
				"--me", "alice",
				"--to", "bob",
				"--project", "peer",
				"--body", "@" + bodyFIFO,
			}
			if strict {
				args = append(args, "--strict")
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runSend(args) }()

			var writer *os.File
			select {
			case result := <-writerCh:
				if result.err != nil {
					t.Fatal(result.err)
				}
				writer = result.file
			case sendErr := <-errCh:
				t.Fatalf("send returned before body read: %v", sendErr)
			case <-time.After(2 * time.Second):
				t.Fatal("send did not reach body read after peer roster validation")
			}
			if err := config.WriteConfig(filepath.Join(peerBase, "meta", "config.json"), config.Config{
				Version: 1,
				Agents:  []string{"carol"},
			}, true); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("fresh roster required")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			sendErr := <-errCh
			_ = stderrWriter.Close()
			os.Stderr = oldStderr
			stderr, readErr := io.ReadAll(stderrReader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			entries, readErr := os.ReadDir(fsq.AgentInboxNew(peerRoot, "bob"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strict {
				if sendErr == nil || !strings.Contains(sendErr.Error(), `handle "bob" not in config.json`) {
					t.Fatalf("strict send error = %v, want fresh roster refusal", sendErr)
				}
				if len(entries) != 0 {
					t.Fatalf("strict send delivered after roster removal: %#v", entries)
				}
			} else {
				if sendErr != nil {
					t.Fatalf("non-strict send failed: %v", sendErr)
				}
				if !strings.Contains(string(stderr), `warning: handle "bob" not in config.json`) {
					t.Fatalf("non-strict stderr = %q, want fresh roster warning", stderr)
				}
				if len(entries) != 1 {
					t.Fatalf("non-strict send delivered %d messages, want 1", len(entries))
				}
			}
		})
	}
}

func TestCrossProjectSendRevalidatesDisappearedConfigAfterBlockingBodyRead(t *testing.T) {
	for _, strict := range []bool{true, false} {
		t.Run(map[bool]string{true: "strict_refuses", false: "non_strict_warns"}[strict], func(t *testing.T) {
			clearDeliveryRootTestEnv(t)
			srcProjectDir := secureTempDirForTest(t)
			srcRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
			peerBase := filepath.Join(secureTempDirForTest(t), "peer")
			peerRoot := filepath.Join(peerBase, "collab")
			if err := fsq.EnsureAgentDirs(srcRoot, "alice"); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
				t.Fatal(err)
			}
			configureSendTestRoot(t, peerBase, "bob")
			rcData, err := json.Marshal(map[string]any{
				"root":    ".agent-mail",
				"project": "source",
				"peers":   map[string]string{"peer": peerBase},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(srcProjectDir, ".amqrc"), rcData, 0o600); err != nil {
				t.Fatal(err)
			}

			bodyFIFO := filepath.Join(secureTempDirForTest(t), "peer-config-removal-body.fifo")
			if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
				t.Fatal(err)
			}
			type openResult struct {
				file *os.File
				err  error
			}
			writerCh := make(chan openResult, 1)
			go func() {
				file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
				writerCh <- openResult{file: file, err: err}
			}()

			oldStderr := os.Stderr
			stderrReader, stderrWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stderr = stderrWriter
			t.Cleanup(func() { os.Stderr = oldStderr })

			args := []string{
				"--root", srcRoot,
				"--me", "alice",
				"--to", "bob",
				"--project", "peer",
				"--body", "@" + bodyFIFO,
			}
			if strict {
				args = append(args, "--strict")
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runSend(args) }()

			var writer *os.File
			select {
			case result := <-writerCh:
				if result.err != nil {
					t.Fatal(result.err)
				}
				writer = result.file
			case sendErr := <-errCh:
				t.Fatalf("send returned before body read: %v", sendErr)
			case <-time.After(2 * time.Second):
				t.Fatal("send did not reach body read after peer roster validation")
			}
			configPath := filepath.Join(peerBase, "meta", "config.json")
			if err := os.Rename(configPath, configPath+".removed"); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("fresh config authority required")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			sendErr := <-errCh
			_ = stderrWriter.Close()
			os.Stderr = oldStderr
			stderr, readErr := io.ReadAll(stderrReader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			entries, readErr := os.ReadDir(fsq.AgentInboxNew(peerRoot, "bob"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			const transition = "selected peer config.json disappeared after initial validation"
			if strict {
				if sendErr == nil || !strings.Contains(sendErr.Error(), transition) {
					t.Fatalf("strict send error = %v, want disappeared-config refusal", sendErr)
				}
				if len(entries) != 0 {
					t.Fatalf("strict send delivered after selected config disappeared: %#v", entries)
				}
			} else {
				if sendErr != nil {
					t.Fatalf("non-strict send failed: %v", sendErr)
				}
				if !strings.Contains(string(stderr), "warning: "+transition) {
					t.Fatalf("non-strict stderr = %q, want fresh disappeared-config warning", stderr)
				}
				if len(entries) != 1 {
					t.Fatalf("non-strict send delivered %d messages, want 1", len(entries))
				}
			}
		})
	}
}

func TestCrossProjectReplyRevalidatesDisappearedBaseConfigAfterBlockingBodyRead(t *testing.T) {
	clearDeliveryRootTestEnv(t)
	sourceProjectDir := secureTempDirForTest(t)
	sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "collab")
	peerBase := filepath.Join(secureTempDirForTest(t), "peer")
	peerRoot := filepath.Join(peerBase, "collab")
	if err := fsq.EnsureAgentDirs(sourceRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, peerBase, "bob")
	rcData, err := json.Marshal(map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers":   map[string]string{"peer": peerBase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceProjectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)
	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "peer-config-disappearance",
		ReplyTo:      "bob@collab",
		ReplyProject: "peer",
		FromProject:  "peer",
	})

	bodyFIFO := filepath.Join(secureTempDirForTest(t), "reply-config-removal-body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--strict",
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		writer = result.file
	case replyErr := <-errCh:
		t.Fatalf("reply returned before body read: %v", replyErr)
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not reach body read after peer roster validation")
	}
	configPath := filepath.Join(peerBase, "meta", "config.json")
	if err := os.Rename(configPath, configPath+".removed"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("fresh config authority required")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	replyErr := <-errCh
	const transition = "selected peer config.json disappeared after initial validation"
	if replyErr == nil || !strings.Contains(replyErr.Error(), transition) {
		t.Fatalf("strict reply error = %v, want disappeared-config refusal", replyErr)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("strict reply delivered %d messages after peer config disappeared", got)
	}
}

func TestCrossProjectSendRevalidatesChangedSourceBaseConfigAfterBlockingBodyRead(t *testing.T) {
	clearDeliveryRootTestEnv(t)
	sourceProjectDir := secureTempDirForTest(t)
	sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
	sourceRoot := filepath.Join(sourceBase, "collab")
	peerBase := filepath.Join(secureTempDirForTest(t), "peer")
	peerRoot := filepath.Join(peerBase, "collab")
	if err := fsq.EnsureAgentDirs(sourceRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, sourceBase, "alice")
	configureSendTestRoot(t, peerBase, "bob")
	rcData, err := json.Marshal(map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers":   map[string]string{"peer": peerBase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceProjectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	bodyFIFO := filepath.Join(secureTempDirForTest(t), "source-config-change-body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runSend([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--to", "bob",
			"--project", "peer",
			"--strict",
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		writer = result.file
	case sendErr := <-errCh:
		t.Fatalf("send returned before body read: %v", sendErr)
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach body read after source roster validation")
	}
	if err := config.WriteConfig(
		filepath.Join(sourceBase, "meta", "config.json"),
		config.Config{Version: 1, Agents: []string{"carol"}},
		true,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("fresh source authority required")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	sendErr := <-errCh
	if sendErr == nil || !strings.Contains(sendErr.Error(), `handle "alice" not in config.json`) {
		t.Fatalf("strict send error = %v, want changed source-roster refusal", sendErr)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("strict send delivered %d messages after source roster changed", got)
	}
}

func TestCrossProjectReplyRevalidatesChangedSourceBaseConfigAfterBlockingBodyRead(t *testing.T) {
	clearDeliveryRootTestEnv(t)
	sourceProjectDir := secureTempDirForTest(t)
	sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
	sourceRoot := filepath.Join(sourceBase, "collab")
	peerBase := filepath.Join(secureTempDirForTest(t), "peer")
	peerRoot := filepath.Join(peerBase, "collab")
	if err := fsq.EnsureAgentDirs(sourceRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, sourceBase, "alice")
	configureSendTestRoot(t, peerBase, "bob")
	rcData, err := json.Marshal(map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers":   map[string]string{"peer": peerBase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceProjectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)
	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "source-roster-change-reply",
		ReplyTo:      "bob@collab",
		ReplyProject: "peer",
		FromProject:  "peer",
	})

	replyErr := runBlockedBodyCommandAfterMutation(
		t,
		"reply-source-config-change",
		func(bodyPath string) error {
			return runReply([]string{
				"--root", sourceRoot,
				"--me", "alice",
				"--id", originalID,
				"--strict",
				"--body", "@" + bodyPath,
			})
		},
		func() {
			if err := config.WriteConfig(
				filepath.Join(sourceBase, "meta", "config.json"),
				config.Config{Version: 1, Agents: []string{"carol"}},
				true,
			); err != nil {
				t.Fatal(err)
			}
		},
	)
	if replyErr == nil || !strings.Contains(replyErr.Error(), `handle "alice" not in config.json`) {
		t.Fatalf("strict reply error = %v, want changed source-roster refusal", replyErr)
	}
	if got := inboxCount(t, peerRoot, "bob"); got != 0 {
		t.Fatalf("strict reply delivered %d messages after source roster changed", got)
	}
}

func TestRoutedSendAndReplyRejectSourceSessionReplacementAfterBlockingBodyRead(t *testing.T) {
	for _, operation := range []string{"send", "reply"} {
		t.Run(operation, func(t *testing.T) {
			clearDeliveryRootTestEnv(t)
			sourceProjectDir := secureTempDirForTest(t)
			sourceBase := filepath.Join(sourceProjectDir, ".agent-mail")
			sourceRoot := filepath.Join(sourceBase, "collab")
			movedSourceRoot := filepath.Join(sourceBase, "collab-authorized")
			replacementRoot := filepath.Join(sourceBase, "replacement")
			peerBase := filepath.Join(secureTempDirForTest(t), "peer")
			peerRoot := filepath.Join(peerBase, "collab")
			for _, root := range []string{sourceRoot, replacementRoot} {
				if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
				t.Fatal(err)
			}
			configureSendTestRoot(t, sourceBase, "alice")
			configureSendTestRoot(t, peerBase, "bob")
			rcData, err := json.Marshal(map[string]any{
				"root":    ".agent-mail",
				"project": "source",
				"peers":   map[string]string{"peer": peerBase},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sourceProjectDir, ".amqrc"), rcData, 0o600); err != nil {
				t.Fatal(err)
			}
			resetAmqrcCache()
			t.Cleanup(resetAmqrcCache)

			originalID := ""
			if operation == "reply" {
				originalID = deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
					From:         "bob",
					To:           []string{"alice"},
					Thread:       "source-session-replacement-reply",
					ReplyTo:      "bob@collab",
					ReplyProject: "peer",
					FromProject:  "peer",
				})
			}
			commandErr := runBlockedBodyCommandAfterMutation(
				t,
				operation+"-source-session-replacement",
				func(bodyPath string) error {
					if operation == "send" {
						return runSend([]string{
							"--root", sourceRoot,
							"--me", "alice",
							"--to", "bob",
							"--project", "peer",
							"--session", "collab",
							"--strict",
							"--body", "@" + bodyPath,
						})
					}
					return runReply([]string{
						"--root", sourceRoot,
						"--me", "alice",
						"--id", originalID,
						"--strict",
						"--body", "@" + bodyPath,
					})
				},
				func() {
					if err := os.Rename(sourceRoot, movedSourceRoot); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacementRoot, sourceRoot); err != nil {
						t.Fatal(err)
					}
				},
			)
			if commandErr == nil || !strings.Contains(commandErr.Error(), "delivery root changed after authorization") {
				t.Fatalf("%s error = %v, want source-root replacement refusal", operation, commandErr)
			}
			if got := inboxCount(t, peerRoot, "bob"); got != 0 {
				t.Fatalf("%s delivered %d messages after source session replacement", operation, got)
			}
		})
	}
}

func TestCrossSessionSendAndReplyRejectSourceSessionReplacementAfterBlockingBodyRead(t *testing.T) {
	for _, operation := range []string{"send", "reply"} {
		t.Run(operation, func(t *testing.T) {
			clearDeliveryRootTestEnv(t)
			baseRoot := filepath.Join(secureTempDirForTest(t), ".agent-mail")
			sourceRoot := filepath.Join(baseRoot, "collab")
			movedSourceRoot := filepath.Join(baseRoot, "collab-authorized")
			replacementRoot := filepath.Join(baseRoot, "replacement")
			targetRoot := filepath.Join(baseRoot, "qa")
			for _, root := range []string{sourceRoot, replacementRoot} {
				if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsq.EnsureAgentDirs(targetRoot, "bob"); err != nil {
				t.Fatal(err)
			}
			configureSendTestRoot(t, baseRoot, "alice", "bob")

			originalID := ""
			if operation == "reply" {
				originalID = deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
					From:    "bob",
					To:      []string{"alice"},
					Thread:  "cross-session-source-replacement-reply",
					ReplyTo: "bob@qa",
				})
			}
			commandErr := runBlockedBodyCommandAfterMutation(
				t,
				operation+"-cross-session-source-replacement",
				func(bodyPath string) error {
					if operation == "send" {
						return runSend([]string{
							"--root", sourceRoot,
							"--me", "alice",
							"--to", "bob",
							"--session", "qa",
							"--strict",
							"--body", "@" + bodyPath,
						})
					}
					return runReply([]string{
						"--root", sourceRoot,
						"--me", "alice",
						"--id", originalID,
						"--strict",
						"--body", "@" + bodyPath,
					})
				},
				func() {
					if err := os.Rename(sourceRoot, movedSourceRoot); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacementRoot, sourceRoot); err != nil {
						t.Fatal(err)
					}
				},
			)
			if commandErr == nil || !strings.Contains(commandErr.Error(), "delivery root changed after authorization") {
				t.Fatalf("%s error = %v, want cross-session source-root replacement refusal", operation, commandErr)
			}
			if got := inboxCount(t, targetRoot, "bob"); got != 0 {
				t.Fatalf("%s delivered %d cross-session messages after source replacement", operation, got)
			}
		})
	}
}

func TestCrossProjectSendPreservesCommittedDeliveryWhenLayoutAlsoChanges(t *testing.T) {
	clearDeliveryRootTestEnv(t)
	srcProjectDir := secureTempDirForTest(t)
	srcRoot := filepath.Join(srcProjectDir, ".agent-mail", "collab")
	peerBase := filepath.Join(secureTempDirForTest(t), "peer")
	peerRoot := filepath.Join(peerBase, "collab")
	if err := fsq.EnsureAgentDirs(srcRoot, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(peerRoot, "bob"); err != nil {
		t.Fatal(err)
	}
	configureSendTestRoot(t, peerBase, "bob")
	rcData, err := json.Marshal(map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers":   map[string]string{"peer": peerBase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcProjectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	originalDeliver := deliverToExistingInbox
	deliverToExistingInbox = func(root *fsq.DeliveryRoot, agent, filename string, data []byte) (string, error) {
		path, deliverErr := originalDeliver(root, agent, filename, data)
		if deliverErr != nil {
			return path, deliverErr
		}
		if removeErr := os.Remove(fsq.AgentDLQCur(peerRoot, agent)); removeErr != nil {
			return path, removeErr
		}
		return path, &fsq.CommittedDurabilityError{
			FinalPath: path,
			Recipient: agent,
			Err:       errors.New("injected post-rename directory sync failure"),
		}
	}
	t.Cleanup(func() { deliverToExistingInbox = originalDeliver })

	sendErr := runSend([]string{
		"--root", srcRoot,
		"--me", "alice",
		"--to", "bob",
		"--project", "peer",
		"--body", "already visible",
	})
	if sendErr == nil {
		t.Fatal("send error = nil, want committed durability warning")
	}
	var committed *fsq.CommittedDurabilityError
	if !errors.As(sendErr, &committed) {
		t.Fatalf("send error lost committed classification after layout change: %T %v", sendErr, sendErr)
	}
	if !strings.Contains(sendErr.Error(), "retrying may duplicate") {
		t.Fatalf("send error = %v, want explicit duplicate warning", sendErr)
	}
	entries, readErr := os.ReadDir(fsq.AgentInboxNew(peerRoot, "bob"))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("committed inbox state = %#v, err=%v", entries, readErr)
	}
}

func TestSendRejectsEscapingMailboxSymlink(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "authorized")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	outsideInbox := filepath.Join(parent, "outside-inbox")
	for _, box := range []string{"tmp", "new"} {
		if err := os.MkdirAll(filepath.Join(outsideInbox, box), 0o700); err != nil {
			t.Fatalf("mkdir outside inbox: %v", err)
		}
	}
	bobInbox := filepath.Join(root, "agents", "bob", "inbox")
	if err := os.RemoveAll(bobInbox); err != nil {
		t.Fatalf("remove bob inbox: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "outside-inbox"), bobInbox); err != nil {
		t.Fatalf("symlink escaping inbox: %v", err)
	}
	clearDeliveryRootTestEnv(t)

	err := runSend([]string{
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "must stay in root",
	})
	if err == nil {
		t.Fatal("send through an escaping mailbox symlink succeeded")
	}
	if entries, readErr := os.ReadDir(filepath.Join(outsideInbox, "new")); readErr != nil {
		t.Fatalf("ReadDir outside inbox: %v", readErr)
	} else if len(entries) != 0 {
		t.Fatalf("escaping mailbox symlink received %d message(s)", len(entries))
	}
}

func TestReplyRejectsSymlinkSwapAfterGuard(t *testing.T) {
	parent := secureTempDirForTest(t)
	authorizedAlias := filepath.Join(parent, "authorized")
	authorizedMoved := filepath.Join(parent, "authorized-moved")
	outside := filepath.Join(parent, "outside")
	for _, root := range []string{authorizedAlias, outside} {
		for _, agent := range []string{"alice", "bob"} {
			if err := fsq.EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs(%s,%s): %v", root, agent, err)
			}
		}
	}
	now := time.Now()
	originalID, err := format.NewMessageID(now)
	if err != nil {
		t.Fatalf("NewMessageID: %v", err)
	}
	original := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "root swap",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "authorize before replying",
	}
	data, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, authorizedAlias, "alice", originalID+".md", data); err != nil {
		t.Fatalf("deliver original: %v", err)
	}
	clearDeliveryRootTestEnv(t)

	bodyFIFO := filepath.Join(parent, "reply-body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runReply([]string{
			"--root", authorizedAlias,
			"--me", "alice",
			"--id", originalID,
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatalf("open FIFO writer: %v", result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("reply returned before reading body: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not reach post-authorization body read")
	}
	if err := os.Rename(authorizedAlias, authorizedMoved); err != nil {
		t.Fatalf("move authorized root: %v", err)
	}
	if err := os.Symlink(outside, authorizedAlias); err != nil {
		t.Fatalf("replace authorized alias: %v", err)
	}
	if _, err := writer.Write([]byte("after authorization")); err != nil {
		t.Fatalf("write FIFO body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close FIFO writer: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
			t.Fatalf("reply error = %v, want post-authorization root-change refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not return after body was released")
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(outside, "bob")); err != nil {
		t.Fatalf("ReadDir outside inbox: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("escaped reply wrote %d message(s) outside authorized root", len(entries))
	}
}

func TestSendRejectsInRootRelativeMailboxSymlink(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "authorized")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	inRootInbox := filepath.Join(root, "mailboxes", "bob")
	for _, box := range []string{"tmp", "new"} {
		if err := os.MkdirAll(filepath.Join(inRootInbox, box), 0o700); err != nil {
			t.Fatalf("mkdir in-root inbox: %v", err)
		}
	}
	bobInbox := filepath.Join(root, "agents", "bob", "inbox")
	if err := os.RemoveAll(bobInbox); err != nil {
		t.Fatalf("remove bob inbox: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "mailboxes", "bob"), bobInbox); err != nil {
		t.Fatalf("symlink in-root inbox: %v", err)
	}
	clearDeliveryRootTestEnv(t)

	if err := runSend([]string{
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "contained symlink",
	}); err == nil {
		t.Fatal("send through contained mailbox symlink succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(inRootInbox, "new"))
	if err != nil {
		t.Fatalf("ReadDir in-root inbox: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused in-root mailbox symlink received %d messages", len(entries))
	}
}

func clearDeliveryRootTestEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{envRoot, envBaseRoot, envSession, "AMQ_GLOBAL_ROOT"} {
		setOptionalEnv(t, key, "", false)
	}
}
