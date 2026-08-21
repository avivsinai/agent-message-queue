package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	applyDropRelDir    = "bridge/drop"
	maxApplyFileSize   = 16 * 1024 * 1024
	applyReceiptRelDir = "bridge/receipts"
)

// runApplyFile authenticates one complete envelope dropped under the local
// bridge drop directory, applies it to the receiver-owned Maildir, and emits
// a local destination_maildir_committed receipt. It never contacts a
// rendezvous or executes anything from the envelope.
func runApplyFile(args []string) error {
	fs := flag.NewFlagSet("amq-bridge apply-file", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootFlag := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	fileFlag := fs.String("file", "", "complete envelope file under <root>/bridge/drop")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return usageError("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*rootFlag) == "" {
		return usageError("--root is required")
	}
	if strings.TrimSpace(*fileFlag) == "" {
		return usageError("--file is required")
	}

	rootPath, err := filepath.Abs(strings.TrimSpace(*rootFlag))
	if err != nil {
		return fmt.Errorf("resolve bridge root: %w", err)
	}
	fileRel, err := applyFileRelPath(rootPath, *fileFlag)
	if err != nil {
		return err
	}
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	raw, err := readApplyFile(root, fileRel)
	if err != nil {
		return fmt.Errorf("read dropped envelope %s: %w", root.DisplayPath(fileRel), err)
	}
	env, err := bridge.UnmarshalEnvelope(raw)
	if err != nil {
		return fmt.Errorf("parse dropped envelope %s: %w", root.DisplayPath(fileRel), err)
	}
	localHost, err := bridge.LoadHostIDFromDeliveryRoot(root)
	if err != nil {
		return err
	}
	destHost, destAgent, err := bridge.ParseAlias(env.DestAlias)
	if err != nil {
		return err
	}
	if destHost != localHost {
		return fmt.Errorf("bridge dest_alias host %q is not local host-id %q", destHost, localHost)
	}
	public, generation, err := bridge.LoadTrustedFromDeliveryRoot(root, env.SourceHost)
	if err != nil {
		return fmt.Errorf("authenticate source host %q: %w", env.SourceHost, err)
	}
	if err := bridge.VerifyEnvelope(env, public, generation); err != nil {
		return fmt.Errorf("authenticate transfer %s: %w", env.TransferID, err)
	}
	applyResult, err := bridge.ApplyEnvelope(root, localHost, destAgent, env)
	if err != nil {
		return fmt.Errorf("apply transfer %s: %w", env.TransferID, err)
	}
	receipt := Receipt{
		Stage:           ReceiptDestinationMaildirCommit,
		TransferID:      env.TransferID,
		PayloadSHA256:   env.PayloadSHA256,
		Replayed:        applyResult.Replayed,
		SourceMessageID: env.SourceMessageID,
		CommittedPath:   applyResult.Path,
		EmittedAt:       time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeBridgeReceipt(root, applyReceiptRelDir, receipt); err != nil {
		return fmt.Errorf("write destination receipt for %s: %w", env.TransferID, err)
	}
	return json.NewEncoder(os.Stdout).Encode(receipt)
}

func applyFileRelPath(rootPath, rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", usageError("--file is required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootPath, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve dropped envelope: %w", err)
	}
	dropPath := filepath.Join(rootPath, applyDropRelDir)
	rel, err := filepath.Rel(dropPath, path)
	if err != nil {
		return "", fmt.Errorf("resolve dropped envelope: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("dropped envelope must be under %s", dropPath)
	}
	return filepath.Join(applyDropRelDir, rel), nil
}

func readApplyFile(root *fsq.DeliveryRoot, rel string) ([]byte, error) {
	file, info, err := root.OpenRegularNoFollow(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if info.Size() > maxApplyFileSize {
		return nil, fmt.Errorf("envelope exceeds %d bytes", maxApplyFileSize)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxApplyFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxApplyFileSize {
		return nil, fmt.Errorf("envelope exceeds %d bytes", maxApplyFileSize)
	}
	return data, nil
}
