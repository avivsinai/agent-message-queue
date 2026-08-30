package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	defaultBridgeBatchSize = 20
	defaultKeyGeneration   = "1"
	maxHTTPBodySize        = 16 * 1024 * 1024
	transfersPath          = "/v1/transfers"
)

// ReceiptStage is deliberately separate from AMQ's consumer-local receipt
// stages. A transport acknowledgement is not evidence of a destination
// Maildir commit.
type ReceiptStage string

const (
	ReceiptTransportAccepted        ReceiptStage = "transport_accepted"
	ReceiptDestinationMaildirCommit ReceiptStage = "destination_maildir_committed"
)

// Receipt is the local, durable observation emitted by amq-bridge. The
// rendezvous service sees only the smaller wireReceipt below.
type Receipt struct {
	Stage           ReceiptStage `json:"stage"`
	TransferID      string       `json:"transfer_id"`
	PayloadSHA256   string       `json:"payload_sha256"`
	Replayed        bool         `json:"replayed,omitempty"`
	SourceMessageID string       `json:"source_message_id,omitempty"`
	CommittedPath   string       `json:"committed_path,omitempty"`
	EmittedAt       string       `json:"emitted_at"`
}

type wireReceipt struct {
	Stage         ReceiptStage `json:"stage"`
	TransferID    string       `json:"transfer_id"`
	PayloadSHA256 string       `json:"payload_sha256"`
}

type transportResponse struct {
	Receipt wireReceipt `json:"receipt"`
}

type pollResponse struct {
	Envelopes []json.RawMessage `json:"envelopes"`
}

type ackRequest struct {
	Receipt wireReceipt `json:"receipt"`
}

// Mode controls the direction(s) handled by one courier cycle.
type Mode string

const (
	ModeBoth Mode = "both"
	ModePush Mode = "push"
	ModePoll Mode = "poll"
)

// Config is the operator-owned bridge configuration. The default local spool
// is <AMQ root>/bridge/outbox/<source handle>/new. Files in that directory
// must be complete AMQ message files; accepted files are moved to its sibling
// sent directory. This is a small bridge spool, not Maildir synchronisation.
type Config struct {
	Root               string
	RendezvousURL      string
	SourceHost         string
	SourceHandle       string
	DestAlias          string
	ReceiveAlias       string
	LocalHost          string
	AllowedDestAliases []string
	AllowedSourceHosts []string
	SpoolDir           string
	ReceiptDir         string
	KeyGeneration      string
	BatchSize          int
	HTTPClient         *http.Client
}

type Courier struct {
	cfg           Config
	client        *http.Client
	allowedDest   map[string]struct{}
	allowedSource map[string]struct{}
	receiveAlias  string
	localHost     string
	localAgent    string
	hostID        string
	identity      *bridge.HostKey
	receiptRelDir string
}

type PushResult struct {
	Receipts []Receipt `json:"receipts,omitempty"`
}

type PollResult struct {
	Receipts []Receipt `json:"receipts,omitempty"`
}

type RunResult struct {
	Push PushResult `json:"push"`
	Poll PollResult `json:"poll"`
}

type spoolItem struct {
	name string
	data []byte
	env  bridge.Envelope
}

// NewCourier validates the static routing policy before any network or
// mailbox operation. In particular, a destination alias is never accepted
// unless it appears in the receiver-owned allowlist.
func NewCourier(cfg Config) (*Courier, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, fmt.Errorf("bridge root is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve bridge root: %w", err)
	}
	cfg.Root = root
	if strings.TrimSpace(cfg.RendezvousURL) == "" {
		return nil, fmt.Errorf("rendezvous URL is required")
	}
	if err := validateRendezvousURL(cfg.RendezvousURL); err != nil {
		return nil, err
	}
	if cfg.KeyGeneration == "" {
		cfg.KeyGeneration = defaultKeyGeneration
	}
	if strings.TrimSpace(cfg.KeyGeneration) == "" || cfg.KeyGeneration != strings.TrimSpace(cfg.KeyGeneration) {
		return nil, fmt.Errorf("key generation must be nonblank and have no surrounding whitespace")
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = defaultBridgeBatchSize
	}
	if cfg.BatchSize < 1 {
		return nil, fmt.Errorf("batch size must be positive")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: 20 * time.Second,
			// A redirect could move an envelope or an ACK to an untrusted host.
			// The rendezvous URL is an explicit operator boundary; do not follow
			// it implicitly.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	if strings.TrimSpace(cfg.DestAlias) == "" {
		return nil, fmt.Errorf("destination alias is required")
	}
	if _, _, err := bridge.ParseAlias(cfg.DestAlias); err != nil {
		return nil, fmt.Errorf("destination alias: %w", err)
	}
	allowedDest, err := aliasSet(cfg.AllowedDestAliases, "destination")
	if err != nil {
		return nil, err
	}
	if len(allowedDest) == 0 {
		return nil, fmt.Errorf("at least one destination alias must be allowlisted")
	}
	if _, ok := allowedDest[cfg.DestAlias]; !ok {
		return nil, fmt.Errorf("destination alias %q is not in the allowlist", cfg.DestAlias)
	}
	receiveAlias := strings.TrimSpace(cfg.ReceiveAlias)
	if receiveAlias == "" {
		receiveAlias = cfg.DestAlias
	} else if _, ok := allowedDest[receiveAlias]; !ok {
		return nil, fmt.Errorf("receive alias %q is not in the allowlist", receiveAlias)
	}
	localHost, localAgent, err := bridge.ParseAlias(receiveAlias)
	if err != nil {
		return nil, fmt.Errorf("receive alias: %w", err)
	}
	if cfg.LocalHost != "" {
		if err := fsq.ValidateHandle(cfg.LocalHost); err != nil {
			return nil, fmt.Errorf("local host: %w", err)
		}
		if cfg.LocalHost != localHost {
			return nil, fmt.Errorf("receive alias host %q does not match local host %q", localHost, cfg.LocalHost)
		}
	}
	allowedSource, err := handleSet(cfg.AllowedSourceHosts, "source host")
	if err != nil {
		return nil, err
	}
	if cfg.SourceHost != "" {
		if err := fsq.ValidateHandle(cfg.SourceHost); err != nil {
			return nil, fmt.Errorf("source host: %w", err)
		}
	}
	if cfg.SourceHandle != "" {
		if err := fsq.ValidateHandle(cfg.SourceHandle); err != nil {
			return nil, fmt.Errorf("source handle: %w", err)
		}
	}
	hostID, err := bridge.LoadHostID(cfg.Root)
	if err != nil {
		return nil, err
	}
	if cfg.SourceHost != "" && cfg.SourceHost != hostID {
		return nil, fmt.Errorf("source host %q does not match bridge host-id %q", cfg.SourceHost, hostID)
	}
	var identity *bridge.HostKey
	if cfg.SourceHost != "" {
		key, identErr := bridge.LoadIdentity(cfg.Root)
		if identErr != nil {
			return nil, identErr
		}
		if key.Generation != cfg.KeyGeneration {
			return nil, fmt.Errorf("identity generation %q does not match --key-generation %q", key.Generation, cfg.KeyGeneration)
		}
		identity = &key
	}
	if cfg.SpoolDir == "" {
		if cfg.SourceHandle == "" {
			cfg.SpoolDir = filepath.Join(cfg.Root, "bridge", "outbox")
		} else {
			cfg.SpoolDir = filepath.Join(cfg.Root, "bridge", "outbox", cfg.SourceHandle, "new")
		}
	} else if cfg.SpoolDir, err = filepath.Abs(cfg.SpoolDir); err != nil {
		return nil, fmt.Errorf("resolve bridge spool: %w", err)
	}
	if cfg.ReceiptDir == "" {
		cfg.ReceiptDir = filepath.Join(cfg.Root, "bridge", "receipts")
	} else if cfg.ReceiptDir, err = filepath.Abs(cfg.ReceiptDir); err != nil {
		return nil, fmt.Errorf("resolve bridge receipt directory: %w", err)
	}
	if _, err := rootRelativePath(cfg.Root, cfg.SpoolDir); err != nil {
		return nil, fmt.Errorf("bridge spool directory: %w", err)
	}
	receiptRelDir, err := rootRelativePath(cfg.Root, cfg.ReceiptDir)
	if err != nil {
		return nil, fmt.Errorf("receipt directory: %w", err)
	}

	return &Courier{
		cfg:           cfg,
		client:        cfg.HTTPClient,
		allowedDest:   allowedDest,
		allowedSource: allowedSource,
		receiveAlias:  receiveAlias,
		localHost:     localHost,
		localAgent:    localAgent,
		hostID:        hostID,
		identity:      identity,
		receiptRelDir: receiptRelDir,
	}, nil
}

func aliasSet(values []string, kind string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(values))
	for _, raw := range values {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, _, err := bridge.ParseAlias(value); err != nil {
				return nil, fmt.Errorf("%s alias %q: %w", kind, value, err)
			}
			set[value] = struct{}{}
		}
	}
	return set, nil
}

func handleSet(values []string, kind string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(values))
	for _, raw := range values {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if err := fsq.ValidateHandle(value); err != nil {
				return nil, fmt.Errorf("%s %q: %w", kind, value, err)
			}
			set[value] = struct{}{}
		}
	}
	return set, nil
}

func rootRelativePath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must stay under the AMQ root: %s", path)
	}
	if rel == "." {
		return "", fmt.Errorf("path must name a directory below the AMQ root")
	}
	return filepath.ToSlash(rel), nil
}

func validateRendezvousURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("rendezvous URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("rendezvous URL must use http or https")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("non-loopback rendezvous URL must use https")
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("rendezvous URL must have a host and no userinfo or fragment")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Courier) requirePushConfig() error {
	if c.cfg.SourceHost == "" {
		return fmt.Errorf("source host is required for push")
	}
	if c.cfg.SourceHandle == "" {
		return fmt.Errorf("source handle is required for push")
	}
	return nil
}

func (c *Courier) openDeliveryRoot() (*fsq.DeliveryRoot, error) {
	identity, err := fsq.SnapshotDeliveryRoot(c.cfg.Root)
	if err != nil {
		return nil, err
	}
	return fsq.OpenDeliveryRoot(c.cfg.Root, identity)
}

// PushOnce posts at most BatchSize complete spool messages. A source file is
// moved to sent only after the rendezvous returns a matching
// transport_accepted receipt. A 200 response with any other stage is an
// error, never a successful drain.
func (c *Courier) PushOnce(ctx context.Context) (PushResult, error) {
	var result PushResult
	if err := c.requirePushConfig(); err != nil {
		return result, err
	}
	items, err := c.readSpool()
	if err != nil {
		return result, err
	}
	if len(items) > c.cfg.BatchSize {
		items = items[:c.cfg.BatchSize]
	}
	if len(items) == 0 {
		return result, nil
	}
	root, err := c.openDeliveryRoot()
	if err != nil {
		return result, err
	}
	defer func() { _ = root.Close() }()

	for _, item := range items {
		receipt, err := c.existingReceipt(root, item.env, ReceiptTransportAccepted)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return result, fmt.Errorf("read transport receipt for %s: %w", item.name, err)
			}
			remote, postErr := c.postEnvelope(ctx, item.env)
			if postErr != nil {
				return result, fmt.Errorf("push %s: %w", item.name, postErr)
			}
			receipt = Receipt{
				Stage:           ReceiptTransportAccepted,
				TransferID:      remote.TransferID,
				PayloadSHA256:   remote.PayloadSHA256,
				SourceMessageID: item.env.SourceMessageID,
				EmittedAt:       time.Now().UTC().Format(time.RFC3339Nano),
			}
			if err := c.writeReceipt(root, receipt); err != nil {
				return result, fmt.Errorf("write transport receipt for %s: %w", item.name, err)
			}
		}
		if receipt.TransferID != item.env.TransferID || !strings.EqualFold(receipt.PayloadSHA256, item.env.PayloadSHA256) {
			return result, fmt.Errorf("transport receipt conflicts with spool item %s", item.name)
		}
		if err := c.moveToSent(item.name, item.data); err != nil {
			return result, fmt.Errorf("archive accepted spool item %s: %w", item.name, err)
		}
		result.Receipts = append(result.Receipts, receipt)
	}
	return result, nil
}

// PollOnce fetches at most BatchSize envelopes, validates their destination
// policy, applies them through internal/bridge, and ACKs only after the local
// Maildir commit succeeds.
func (c *Courier) PollOnce(ctx context.Context) (PollResult, error) {
	var result PollResult
	if err := c.guardPollIdentity(); err != nil {
		return result, err
	}
	envelopes, err := c.pollEnvelopes(ctx)
	if err != nil {
		return result, err
	}
	if len(envelopes) == 0 {
		return result, nil
	}
	root, err := c.openDeliveryRoot()
	if err != nil {
		return result, err
	}
	defer func() { _ = root.Close() }()

	for _, env := range envelopes {
		if env.DestAlias != c.receiveAlias {
			return result, fmt.Errorf("inbound destination alias %q does not match configured receiver %q", env.DestAlias, c.receiveAlias)
		}
		if _, ok := c.allowedDest[env.DestAlias]; !ok {
			return result, fmt.Errorf("inbound destination alias %q is not allowlisted", env.DestAlias)
		}
		if _, ok := c.allowedSource[env.SourceHost]; !ok {
			return result, fmt.Errorf("inbound source host %q is not allowlisted", env.SourceHost)
		}
		pub, generation, err := bridge.LoadTrusted(c.cfg.Root, env.SourceHost)
		if err != nil {
			return result, fmt.Errorf("authenticate source host %q: %w", env.SourceHost, err)
		}
		if err := bridge.VerifyEnvelope(env, pub, generation); err != nil {
			return result, fmt.Errorf("authenticate transfer %s: %w", env.TransferID, err)
		}
		applyResult, err := bridge.ApplyEnvelope(root, c.localHost, c.localAgent, env)
		if err != nil {
			return result, fmt.Errorf("apply transfer %s: %w", env.TransferID, err)
		}
		receipt := Receipt{
			Stage:           ReceiptDestinationMaildirCommit,
			TransferID:      env.TransferID,
			PayloadSHA256:   env.PayloadSHA256,
			Replayed:        applyResult.Replayed,
			CommittedPath:   applyResult.Path,
			SourceMessageID: env.SourceMessageID,
			EmittedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := c.writeReceipt(root, receipt); err != nil {
			return result, fmt.Errorf("write destination receipt for %s: %w", env.TransferID, err)
		}
		if err := c.ackEnvelope(ctx, env); err != nil {
			return result, fmt.Errorf("ack transfer %s: %w", env.TransferID, err)
		}
		result.Receipts = append(result.Receipts, receipt)
	}
	return result, nil
}

// RunOnce executes one bounded push/poll cycle. It never treats the push
// receipt as proof that a destination Maildir was committed.
func (c *Courier) RunOnce(ctx context.Context, mode Mode) (RunResult, error) {
	var result RunResult
	if mode != ModeBoth && mode != ModePush && mode != ModePoll {
		return result, fmt.Errorf("invalid bridge mode %q", mode)
	}
	if mode == ModeBoth || mode == ModePush {
		push, err := c.PushOnce(ctx)
		result.Push = push
		if err != nil {
			return result, err
		}
	}
	if mode == ModeBoth || mode == ModePoll {
		poll, err := c.PollOnce(ctx)
		result.Poll = poll
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (c *Courier) readSpool() ([]spoolItem, error) {
	if err := os.MkdirAll(c.cfg.SpoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("create bridge spool: %w", err)
	}
	entries, err := os.ReadDir(c.cfg.SpoolDir)
	if err != nil {
		return nil, fmt.Errorf("read bridge spool: %w", err)
	}
	// os.ReadDir sorts by filename, but sort again after filtering to make the
	// ordering explicit if the directory source changes in the future.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	items := make([]spoolItem, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		if err := fsq.ValidateMessageFilename(name); err != nil {
			return nil, fmt.Errorf("spool file %q: %w", name, err)
		}
		path := filepath.Join(c.cfg.SpoolDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("stat spool file %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("spool file %q is not a regular non-symlink file", name)
		}
		file, fileInfo, err := fsq.OpenRegularNoFollow(path)
		if err != nil {
			return nil, fmt.Errorf("open spool file %q: %w", name, err)
		}
		if fileInfo.Size() > format.MaxMessageSize {
			_ = file.Close()
			return nil, fmt.Errorf("spool file %q exceeds maximum message size", name)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, format.MaxMessageSize+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read spool file %q: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close spool file %q: %w", name, closeErr)
		}
		if len(data) > format.MaxMessageSize {
			return nil, fmt.Errorf("spool file %q exceeds maximum message size", name)
		}
		message, err := format.ParseMessage(data)
		if err != nil {
			return nil, fmt.Errorf("parse spool file %q: %w", name, err)
		}
		if message.Header.ID == "" || strings.TrimSpace(message.Header.ID) != message.Header.ID {
			return nil, fmt.Errorf("spool file %q has invalid message id", name)
		}
		if message.Header.Thread == "" || strings.TrimSpace(message.Header.Thread) != message.Header.Thread {
			return nil, fmt.Errorf("spool file %q has invalid thread id", name)
		}
		if message.Header.From != c.cfg.SourceHandle {
			return nil, fmt.Errorf("spool file %q sender %q does not match source handle %q", name, message.Header.From, c.cfg.SourceHandle)
		}
		destAlias, destErr := c.destAliasForSpool(path)
		if destErr != nil {
			return nil, fmt.Errorf("spool file %q dest: %w", name, destErr)
		}
		env := envelopeForMessage(c.cfg, destAlias, message.Header.ID, message.Header.Thread, data)
		if c.identity == nil {
			return nil, fmt.Errorf("push requires a local host identity")
		}
		if err := bridge.SignEnvelope(&env, *c.identity); err != nil {
			return nil, fmt.Errorf("sign spool file %q: %w", name, err)
		}
		items = append(items, spoolItem{name: name, data: data, env: env})
	}
	return items, nil
}

func envelopeForMessage(cfg Config, destAlias, messageID, threadID string, payload []byte) bridge.Envelope {
	digest := sha256.Sum256(payload)
	return bridge.Envelope{
		Version:         bridge.EnvelopeVersion,
		TransferID:      bridge.DeriveTransferID(cfg.SourceHost, cfg.SourceHandle, messageID, destAlias),
		SourceHost:      cfg.SourceHost,
		SourceHandle:    cfg.SourceHandle,
		DestAlias:       destAlias,
		SourceMessageID: messageID,
		ThreadID:        threadID,
		PayloadSHA256:   hex.EncodeToString(digest[:]),
		KeyGeneration:   cfg.KeyGeneration,
		Payload:         payload,
	}
}

func (c *Courier) destAliasForSpool(messagePath string) (string, error) {
	path := bridge.DestSidecarPath(messagePath)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return c.cfg.DestAlias, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("dest sidecar %q is not a regular non-symlink file", path)
	}
	data, err := fsq.ReadRegularNoFollow(path)
	if err != nil {
		return "", err
	}
	alias := strings.TrimSpace(string(data))
	if _, _, err := bridge.ParseAlias(alias); err != nil {
		return "", err
	}
	if _, ok := c.allowedDest[alias]; !ok {
		return "", fmt.Errorf("destination alias %q is not allowlisted", alias)
	}
	return alias, nil
}

func (c *Courier) postEnvelope(ctx context.Context, env bridge.Envelope) (wireReceipt, error) {
	body, err := bridge.MarshalEnvelope(env)
	if err != nil {
		return wireReceipt{}, err
	}
	var response transportResponse
	if err := c.requestJSON(ctx, http.MethodPost, transfersPath, nil, body, &response); err != nil {
		return wireReceipt{}, err
	}
	if err := validateWireReceipt(response.Receipt, ReceiptTransportAccepted, env); err != nil {
		return wireReceipt{}, err
	}
	return response.Receipt, nil
}

func (c *Courier) pollEnvelopes(ctx context.Context) ([]bridge.Envelope, error) {
	query := url.Values{}
	query.Set("dest_alias", c.receiveAlias)
	query.Set("limit", fmt.Sprintf("%d", c.cfg.BatchSize))
	var response pollResponse
	if err := c.requestJSON(ctx, http.MethodGet, transfersPath, query, nil, &response); err != nil {
		return nil, err
	}
	if len(response.Envelopes) > c.cfg.BatchSize {
		return nil, fmt.Errorf("rendezvous returned %d envelopes above batch size %d", len(response.Envelopes), c.cfg.BatchSize)
	}
	envelopes := make([]bridge.Envelope, 0, len(response.Envelopes))
	for i, raw := range response.Envelopes {
		env, err := bridge.UnmarshalEnvelope(raw)
		if err != nil {
			return nil, fmt.Errorf("decode polled envelope %d: %w", i, err)
		}
		envelopes = append(envelopes, env)
	}
	return envelopes, nil
}

func (c *Courier) ackEnvelope(ctx context.Context, env bridge.Envelope) error {
	path := transfersPath + "/" + url.PathEscape(env.TransferID) + "/ack"
	body, err := json.Marshal(ackRequest{Receipt: wireReceipt{
		Stage:         ReceiptDestinationMaildirCommit,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}})
	if err != nil {
		return err
	}
	var response transportResponse
	if err := c.requestJSON(ctx, http.MethodPost, path, nil, body, &response); err != nil {
		return err
	}
	if err := validateWireReceipt(response.Receipt, ReceiptDestinationMaildirCommit, env); err != nil {
		return err
	}
	return nil
}

func validateWireReceipt(receipt wireReceipt, want ReceiptStage, env bridge.Envelope) error {
	if receipt.Stage != want {
		return fmt.Errorf("unexpected rendezvous receipt stage %q, want %q", receipt.Stage, want)
	}
	if receipt.TransferID != env.TransferID {
		return fmt.Errorf("rendezvous receipt transfer_id %q does not match %q", receipt.TransferID, env.TransferID)
	}
	if !strings.EqualFold(receipt.PayloadSHA256, env.PayloadSHA256) {
		return fmt.Errorf("rendezvous receipt digest does not match transfer %q", env.TransferID)
	}
	return nil
}

func (c *Courier) requestJSON(ctx context.Context, method, requestPath string, query url.Values, body []byte, out any) error {
	endpoint, err := rendezvousEndpoint(c.cfg.RendezvousURL, requestPath, query)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodySize+1))
	if err != nil {
		return err
	}
	if len(responseBody) > maxHTTPBodySize {
		return fmt.Errorf("rendezvous response exceeds %d bytes", maxHTTPBodySize)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("rendezvous %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if out == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode rendezvous response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("rendezvous response has trailing JSON")
		}
		return fmt.Errorf("decode rendezvous response trailer: %w", err)
	}
	return nil
}

func rendezvousEndpoint(raw, requestPath string, query url.Values) (string, error) {
	base, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse rendezvous URL: %w", err)
	}
	if err := validateRendezvousURL(raw); err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + requestPath
	base.RawPath = ""
	merged := base.Query()
	for key, values := range query {
		merged[key] = values
	}
	base.RawQuery = merged.Encode()
	return base.String(), nil
}

func (c *Courier) guardPollIdentity() error {
	if len(c.allowedSource) == 0 {
		return fmt.Errorf("poll requires a non-empty --allow-source-host allowlist")
	}
	if c.localHost != c.hostID {
		return fmt.Errorf("poll receive alias host %q is not local host-id %q; set --receive-alias", c.localHost, c.hostID)
	}
	if c.cfg.SourceHost != "" && c.localHost != c.cfg.SourceHost {
		return fmt.Errorf("poll receive alias host %q is not local source host %q; set --receive-alias", c.localHost, c.cfg.SourceHost)
	}
	return nil
}

func (c *Courier) existingReceipt(root *fsq.DeliveryRoot, env bridge.Envelope, stage ReceiptStage) (Receipt, error) {
	path := filepath.Join(c.receiptRelDir, receiptFilename(env.TransferID, stage))
	data, err := root.ReadRegularNoFollow(path)
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("parse receipt %s: %w", root.DisplayPath(path), err)
	}
	if receipt.Stage != stage || receipt.TransferID != env.TransferID || !strings.EqualFold(receipt.PayloadSHA256, env.PayloadSHA256) {
		return Receipt{}, fmt.Errorf("receipt %s conflicts with transfer", root.DisplayPath(path))
	}
	return receipt, nil
}

func (c *Courier) writeReceipt(root *fsq.DeliveryRoot, receipt Receipt) error {
	return writeBridgeReceipt(root, c.receiptRelDir, receipt)
}

func writeBridgeReceipt(root *fsq.DeliveryRoot, receiptRelDir string, receipt Receipt) error {
	if receipt.EmittedAt == "" {
		receipt.EmittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	filename := receiptFilename(receipt.TransferID, receipt.Stage)
	path := filepath.Join(receiptRelDir, filename)
	if _, err := root.WriteFileExclusive(receiptRelDir, filename, data, 0o600); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, err := root.ReadRegularNoFollow(path)
	if err != nil {
		return err
	}
	var prior Receipt
	if err := json.Unmarshal(existing, &prior); err != nil {
		return fmt.Errorf("parse existing receipt %s: %w", root.DisplayPath(path), err)
	}
	if prior.Stage != receipt.Stage || prior.TransferID != receipt.TransferID || !strings.EqualFold(prior.PayloadSHA256, receipt.PayloadSHA256) {
		return fmt.Errorf("receipt %s conflicts with transfer", root.DisplayPath(path))
	}
	return nil
}

func receiptFilename(transferID string, stage ReceiptStage) string {
	return transferID + "__" + string(stage) + ".json"
}

func (c *Courier) moveToSent(name string, data []byte) error {
	sentDir := filepath.Join(filepath.Dir(c.cfg.SpoolDir), "sent")
	if err := os.MkdirAll(sentDir, 0o700); err != nil {
		return err
	}
	source := filepath.Join(c.cfg.SpoolDir, name)
	destination := filepath.Join(sentDir, name)
	if err := os.Link(source, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := fsq.ReadRegularNoFollow(destination)
		if readErr != nil {
			return readErr
		}
		if string(existing) != string(data) {
			return fmt.Errorf("sent archive %s conflicts with source bytes", destination)
		}
	}
	if err := syncDirectory(sentDir); err != nil {
		return fmt.Errorf("sync sent archive: %w", err)
	}
	if err := os.Remove(source); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(bridge.DestSidecarPath(source)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDirectory(c.cfg.SpoolDir); err != nil {
		return fmt.Errorf("sync bridge spool: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
