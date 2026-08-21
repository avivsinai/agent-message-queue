package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	defaultSourceHost   = "grok"
	defaultSourceHandle = "codex"
	defaultDestAlias    = "mac/claude"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "write" {
		fmt.Fprintln(os.Stderr, "usage: amq-bot-envelope-hop-probe-tool write --root ROOT --output FILE")
		os.Exit(2)
	}
	if err := runWrite(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "amq-bot-envelope-hop-probe-tool:", err)
		os.Exit(1)
	}
}

func runWrite(args []string) error {
	fs := flag.NewFlagSet("amq-bot-envelope-hop-probe-tool write", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootFlag := fs.String("root", "", "G bridge root containing host-id and identity")
	outputFlag := fs.String("output", "", "exclusive envelope output path")
	transferIDFlag := fs.String("transfer-id", "", "probe transfer id; generated when empty")
	sourceHostFlag := fs.String("source-host", defaultSourceHost, "source host alias")
	sourceHandleFlag := fs.String("source-handle", defaultSourceHandle, "source AMQ handle")
	destAliasFlag := fs.String("dest-alias", defaultDestAlias, "receiver-owned destination alias")
	threadFlag := fs.String("thread", "", "opaque AMQ thread id; default probe/<transfer-id>")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	rootPath, err := requiredAbsolutePath("--root", *rootFlag)
	if err != nil {
		return err
	}
	outputPath, err := requiredAbsolutePath("--output", *outputFlag)
	if err != nil {
		return err
	}
	sourceHost := strings.TrimSpace(*sourceHostFlag)
	sourceHandle := strings.TrimSpace(*sourceHandleFlag)
	destAlias := strings.TrimSpace(*destAliasFlag)
	if err := fsq.ValidateHandle(sourceHost); err != nil {
		return fmt.Errorf("source host: %w", err)
	}
	if err := fsq.ValidateHandle(sourceHandle); err != nil {
		return fmt.Errorf("source handle: %w", err)
	}
	_, destAgent, err := bridge.ParseAlias(destAlias)
	if err != nil {
		return err
	}

	hostID, err := bridge.LoadHostID(rootPath)
	if err != nil {
		return err
	}
	if hostID != sourceHost {
		return fmt.Errorf("source host %q does not match G bridge host-id %q", sourceHost, hostID)
	}
	key, err := bridge.LoadIdentity(rootPath)
	if err != nil {
		return err
	}
	transferID := strings.TrimSpace(*transferIDFlag)
	if transferID == "" {
		transferID, err = randomID("xfer-probe-")
		if err != nil {
			return fmt.Errorf("generate transfer id: %w", err)
		}
	}
	if err := fsq.ValidateHandle(transferID); err != nil {
		return fmt.Errorf("transfer id: %w", err)
	}
	messageID := "probe-message-" + strings.TrimPrefix(transferID, "xfer-probe-")
	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		threadID = "probe/" + transferID
	}
	payload, err := probeMessage(messageID, threadID, sourceHandle, destAgent, transferID)
	if err != nil {
		return fmt.Errorf("marshal probe payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	env := bridge.Envelope{
		Version:         bridge.EnvelopeVersion,
		TransferID:      transferID,
		SourceHost:      sourceHost,
		SourceHandle:    sourceHandle,
		DestAlias:       destAlias,
		SourceMessageID: messageID,
		ThreadID:        threadID,
		PayloadSHA256:   hex.EncodeToString(digest[:]),
		KeyGeneration:   key.Generation,
		Payload:         payload,
	}
	if err := bridge.SignEnvelope(&env, key); err != nil {
		return fmt.Errorf("sign envelope: %w", err)
	}
	raw, err := bridge.MarshalEnvelope(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if err := writeExclusive(outputPath, raw); err != nil {
		return fmt.Errorf("write envelope: %w", err)
	}

	fmt.Printf("BOT_ENVELOPE_HOP_TRANSFER_ID=%s\n", transferID)
	fmt.Printf("BOT_ENVELOPE_HOP_SOURCE=%s\n", outputPath)
	fmt.Printf("BOT_ENVELOPE_HOP_SOURCE_HOST=%s\n", sourceHost)
	fmt.Printf("BOT_ENVELOPE_HOP_DEST_ALIAS=%s\n", destAlias)
	fmt.Printf("BOT_ENVELOPE_HOP_KEY_GENERATION=%s\n", key.Generation)
	fmt.Printf("BOT_ENVELOPE_HOP_PUBLIC=%s\n", hex.EncodeToString(key.Public()))
	fmt.Println("BOT_ENVELOPE_HOP_NEXT=move this envelope file with Grok Bot; do not paste its contents")
	return nil
}

func requiredAbsolutePath(name, raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	return abs, nil
}

func randomID(prefix string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := prefix + hex.EncodeToString(nonce[:])
	if err := fsq.ValidateHandle(id); err != nil {
		return "", err
	}
	return id, nil
}

func probeMessage(messageID, threadID, sourceHandle, destAgent, transferID string) ([]byte, error) {
	return (format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      messageID,
			From:    sourceHandle,
			To:      []string{destAgent},
			Thread:  threadID,
			Subject: "AMQ envelope hop probe",
			Created: time.Now().UTC().Format(time.RFC3339Nano),
			Kind:    format.KindStatus,
		},
		Body: "AMQ envelope hop probe payload " + transferID,
	}).Marshal()
}

func writeExclusive(path string, data []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".envelope-hop-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	keep = true
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
